package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/sessfiles"
	"github.com/PiDmitrius/klax/internal/session"
	"github.com/PiDmitrius/klax/internal/turnaudit"
)

type apiFixture struct {
	d      *daemon
	s      *uiServer
	dir    string
	klaxID string
}

func newAPIFixture(t *testing.T, start, finish, backend string) *apiFixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KLAX_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("KLAX_API_TEST_DIR", dir)
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	d := newTestDaemon(t)
	var err error
	d.store, err = session.LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	d.runners = make(map[runnerKey]*sessionRunner)
	d.cfg.Audit = &config.AuditConfig{Turn: &config.AuditTurnConfig{}}
	hook := func(phase, mode string) *config.AuditHookConfig {
		if mode == "" {
			return nil
		}
		script := "cat > \"$KLAX_API_TEST_DIR/" + phase + ".json\"\n"
		if mode == "block" {
			script += "touch \"$KLAX_API_TEST_DIR/" + phase + ".entered\"\nwhile [ -d \"$KLAX_API_TEST_DIR\" ] && [ ! -f \"$KLAX_API_TEST_DIR/" + phase + ".release\" ]; do sleep 0.01; done\n"
		}
		if mode == "fail" {
			script += "exit 1\n"
		}
		return &config.AuditHookConfig{Command: []string{"/bin/sh", "-c", script}}
	}
	d.cfg.Audit.Turn.Start = hook("start", start)
	d.cfg.Audit.Turn.Finish = hook("finish", finish)
	script := "#!/bin/sh\ncat > \"$KLAX_API_TEST_DIR/prompt\"\nprintf '%s\\n' \"$@\" > \"$KLAX_API_TEST_DIR/args\"\nprintf 'x\\n' >> \"$KLAX_API_TEST_DIR/runs\"\ntouch \"$KLAX_API_TEST_DIR/backend.entered\"\n"
	if backend == "block" {
		script += "while [ -d \"$KLAX_API_TEST_DIR\" ] && [ ! -f \"$KLAX_API_TEST_DIR/backend.release\" ]; do sleep 0.01; done\n"
	}
	script += "printf '%s' \"$KLAX_ID\" > \"$KLAX_API_TEST_DIR/session-env\"\n"
	if backend == "fail" {
		script += "exit 1\n"
	} else {
		script += "printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}'\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	sess := d.store.New("user:test", "test", dir, session.ScopeDefaults{Backend: "codex"})
	f := &apiFixture{d: d, s: &uiServer{d: d, tokens: map[string]uiAccess{"access": {User: "test"}, "other": {User: "other"}}}, dir: dir, klaxID: sess.KlaxID}
	t.Cleanup(func() {
		for _, phase := range []string{"start", "finish", "backend"} {
			if err := os.WriteFile(filepath.Join(dir, phase+".release"), nil, 0600); err != nil {
				t.Errorf("release %s: %v", phase, err)
			}
		}
		until := time.Now().Add(5 * time.Second)
		for {
			busy := false
			d.runnersMu.Lock()
			for _, sr := range d.runners {
				sr.mu.Lock()
				busy = busy || sr.processing || len(sr.queue) > 0
				sr.mu.Unlock()
			}
			d.runnersMu.Unlock()
			if !busy {
				break
			}
			if time.Now().After(until) {
				t.Error("queue did not settle")
				break
			}
			time.Sleep(time.Millisecond)
		}
		// Deleted sessions leave the runner map before their workers finish.
		drained := make(chan struct{})
		go func() {
			d.drainWg.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Error("workers did not settle")
		}
	})
	return f
}
func (f *apiFixture) request(path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token == "" {
		token = "access"
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, r)
	return w
}
func (f *apiFixture) send(boundary, nonce string) *httptest.ResponseRecorder {
	return f.request("/api/send", fmt.Sprintf(`{"klax_id":%q,"text":"hello","return_on":%q,"nonce":%q}`, f.klaxID, boundary, nonce), "")
}
func (f *apiFixture) entered(t *testing.T, phase string) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(f.dir, phase+".entered")); err == nil {
			return
		}
		if time.Now().After(until) {
			t.Fatalf("%s not entered", phase)
		}
		time.Sleep(time.Millisecond)
	}
}
func (f *apiFixture) release(t *testing.T, phase string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, phase+".release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}
func asyncResponse(fn func() *httptest.ResponseRecorder) <-chan *httptest.ResponseRecorder {
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() { ch <- fn() }()
	return ch
}
func response(t *testing.T, ch <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-ch:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP response did not finish")
		return nil
	}
}
func blocked(t *testing.T, ch <-chan *httptest.ResponseRecorder) {
	t.Helper()
	select {
	case w := <-ch:
		t.Fatalf("premature response: %d %s", w.Code, w.Body.String())
	case <-time.After(30 * time.Millisecond):
	}
}
func eventResponse(t *testing.T, w *httptest.ResponseRecorder, phase, status string) turnaudit.Event {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	var event turnaudit.Event
	if err := json.Unmarshal(w.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Event != "turn."+phase {
		t.Fatalf("event = %s", event.Event)
	}
	if status != "" && (event.Turn.Result == nil || event.Turn.Result.Status != status) {
		t.Fatalf("result = %+v", event.Turn.Result)
	}
	return event
}
func errorResponse(t *testing.T, w *httptest.ResponseRecorder, code string) {
	t.Helper()
	var body struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	if w.Code < 400 || body.Error.Code != code {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
}

func TestAPIWaitBoundariesAndSharedHookSnapshot(t *testing.T) {
	f := newAPIFixture(t, "block", "block", "block")
	if w := f.send("queued", "same"); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.entered(t, "start")
	start := asyncResponse(func() *httptest.ResponseRecorder { return f.send("start", "same") })
	finish := asyncResponse(func() *httptest.ResponseRecorder { return f.send("finish", "same") })
	blocked(t, start)
	blocked(t, finish)
	if _, err := os.Stat(filepath.Join(f.dir, "backend.entered")); err == nil {
		t.Fatal("backend passed blocked start gate")
	}
	f.release(t, "start")
	begin := eventResponse(t, response(t, start), "start", "")
	raw, err := os.ReadFile(filepath.Join(f.dir, "start.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(begin)
	var captured turnaudit.Event
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(captured)
	if !bytes.Equal(want, got) {
		t.Fatal("start hook/API snapshots differ")
	}
	f.entered(t, "backend")
	blocked(t, finish)
	f.release(t, "backend")
	f.entered(t, "finish")
	blocked(t, finish)
	f.release(t, "finish")
	end := eventResponse(t, response(t, finish), "finish", "success")
	env, err := os.ReadFile(filepath.Join(f.dir, "session-env"))
	if err != nil || string(env) != fmt.Sprint(f.klaxID) {
		t.Fatalf("backend session environment = %q, want %s, error = %v", env, f.klaxID, err)
	}
	raw, err = os.ReadFile(filepath.Join(f.dir, "finish.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ = json.Marshal(end)
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	got, _ = json.Marshal(captured)
	if !bytes.Equal(want, got) {
		t.Fatal("finish hook/API snapshots differ")
	}
	if begin.Turn.ID != end.Turn.ID {
		t.Fatal("turn identity changed")
	}
	eventResponse(t, f.send("start", "same"), "start", "")
	eventResponse(t, f.send("finish", "same"), "finish", "success")
	runs, _ := os.ReadFile(filepath.Join(f.dir, "runs"))
	if string(runs) != "x\n" {
		t.Fatalf("duplicate execution: %q", runs)
	}
}

func TestAPIImmediateFinishWithoutHooks(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	for i := 0; i < 8; i++ {
		w := f.request("/api/send", fmt.Sprintf(`{"klax_id":%q,"text":"hello","return_on":"finish"}`, f.klaxID), "")
		ev := eventResponse(t, w, "finish", "success")
		if ev.Turn.Result.Output.Text != "done" {
			t.Fatal("missing backend output")
		}
	}
	turns, err := f.d.getRunner("user:test", f.klaxID).store.InboundLog()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, turn := range turns {
		if turn.Nonce == "" || seen[turn.Nonce] {
			t.Fatal("nonce not unique")
		}
		seen[turn.Nonce] = true
	}
	if len(turns) != 8 {
		t.Fatal(len(turns))
	}
}

func TestAPIErrorsAndAbort(t *testing.T) {
	for _, tc := range []struct{ start, finish, backend, code, status string }{
		{start: "fail", code: "audit-start-failed"}, {backend: "fail", status: "error"}, {finish: "fail", status: "success"}, {backend: "block", status: "aborted"},
	} {
		t.Run(tc.start+tc.finish+tc.backend, func(t *testing.T) {
			f := newAPIFixture(t, tc.start, tc.finish, tc.backend)
			ch := asyncResponse(func() *httptest.ResponseRecorder { return f.send("finish", "n") })
			if tc.backend == "block" {
				f.entered(t, "backend")
				f.d.abortSession("user:test", f.klaxID, false)
			}
			w := response(t, ch)
			if tc.code != "" {
				errorResponse(t, w, tc.code)
				errorResponse(t, f.send("start", "n"), tc.code)
				return
			}
			eventResponse(t, w, "finish", tc.status)
			if tc.finish == "fail" && !strings.Contains(w.Body.String(), `"code":"audit-finish-failed"`) {
				t.Fatal("missing warning")
			}
		})
	}
}

func TestAPIQueuedAbortAndDeleteWakeWaiters(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(fmt.Sprint(closing), func(t *testing.T) {
			f := newAPIFixture(t, "block", "", "")
			f.send("queued", "first")
			f.entered(t, "start")
			f.send("queued", "second")
			wait := asyncResponse(func() *httptest.ResponseRecorder { return f.send("finish", "second") })
			blocked(t, wait)
			f.d.abortSession("user:test", f.klaxID, closing)
			code := "aborted"
			if closing {
				code = "session-deleted"
			}
			errorResponse(t, response(t, wait), code)
			if closing {
				errorResponse(t, f.send("finish", "first"), "session-deleted")
			}
			f.release(t, "start")
		})
	}
}

func TestAPINonceUnavailableAndValidation(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	sr := f.d.getRunner("user:test", f.klaxID)
	if _, _, _, _, err := sr.store.Enqueue("ui:test", "", "historical", "hello", nil); err != nil {
		t.Fatal(err)
	}
	if w := f.send("queued", "historical"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	errorResponse(t, f.send("finish", "historical"), "result-unavailable")
	for _, fields := range []string{`"nonce":""`, `"nonce":null`, `"nonce":1`, `"return_on":""`, `"return_on":true`, `"return_on":"later"`} {
		w := f.request("/api/send", fmt.Sprintf(`{"klax_id":%q,"text":"hello",%s}`, f.klaxID, fields), "")
		if w.Code != 400 {
			t.Fatalf("%s: %d", fields, w.Code)
		}
	}
	turns, _ := sr.store.InboundLog()
	if len(turns) != 1 {
		t.Fatal("invalid request queued")
	}
}

func TestAPIRejectsMalformedRequestsWithoutMutations(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	before, _ := json.Marshal(f.d.store.SessionsFor("user:test"))
	for _, c := range []struct{ path, fields string }{
		{"/api/new", `"model":"wrong"`},
		{"/api/settings", `"model":"wrong"`},
		{"/api/settings", `"tty":null`},
		{"/api/settings", `"groups":null`},
		{"/api/settings", `"groups":[null]`},
		{"/api/send", `"text":"hello","session":"wrong"`},
		{"/api/abort", `"extra":true`},
		{"/api/cancel", `"turn_seq":1,"extra":true`},
		{"/api/read", `"read_pos":"9.2","extra":true`},
		{"/api/rename", `"name":"changed","extra":true`},
		{"/api/reorder", `"tabs":[],"order":[]`},
		{"/api/reorder", `"tabs":[null]`},
		{"/api/close", `"extra":true`},
		{"/api/changes", `"after":"","extra":true`},
		{"/api/models/refresh", `"backend":"codex","extra":true`},
		{"/api/system/update", `"tag":"v0.9.3","extra":true`},
		{"/api/system/check", `"extra":true`},
	} {
		t.Run(c.path+"/"+c.fields, func(t *testing.T) {
			body := fmt.Sprintf(`{"klax_id":%q,%s}`, f.klaxID, c.fields)
			switch c.path {
			case "/api/new", "/api/reorder", "/api/changes", "/api/models/refresh", "/api/system/update", "/api/system/check":
				body = "{" + c.fields + "}"
			}
			w := f.request(c.path, body, "")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
			}
			errorResponse(t, w, "bad-request")
		})
	}
	for _, body := range []string{"null", "[]", `{"name":"changed"} {}`, `{"name":"changed"} garbage`} {
		w := f.request("/api/new", body, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	after, _ := json.Marshal(f.d.store.SessionsFor("user:test"))
	if !bytes.Equal(before, after) {
		t.Fatalf("rejected requests changed sessions: %s -> %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "backend.entered")); !os.IsNotExist(err) {
		t.Fatal("rejected request started a backend", err)
	}
}

func TestAPISessionMutationsRollbackFailedSaveAndRetry(t *testing.T) {
	for _, c := range []struct{ path, code, token string }{
		{"/api/settings", "settings-save-failed", ""},
		{"/api/rename", "settings-save-failed", ""},
		{"/api/read", "read-save-failed", ""},
		{"/api/read", "read-save-failed", "reader"},
		{"/api/reorder", "reorder-save-failed", ""},
		{"/api/close", "close-save-failed", ""},
	} {
		t.Run(c.path+"/"+c.token, func(t *testing.T) {
			f := newAPIFixture(t, "", "", "")
			f.s.tokens["reader"] = uiAccess{User: "test", ReadOnly: true}
			second := f.d.store.New("user:test", "second", f.dir, session.ScopeDefaults{Backend: "codex"})
			if err := f.d.store.Save(); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(f.d.store.SessionsFor("user:test"))
			keep := f.d.sessionStore("user:test", f.klaxID).Path("keep.txt")
			if err := os.MkdirAll(filepath.Dir(keep), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(session.StoreDir(), "sessions.json")
			if err := os.Rename(path, path+".before"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"klax_id":%q}`, f.klaxID)
			switch c.path {
			case "/api/settings", "/api/rename":
				body = fmt.Sprintf(`{"klax_id":%q,"name":"changed"}`, f.klaxID)
			case "/api/read":
				body = fmt.Sprintf(`{"klax_id":%q,"read_pos":"9.2"}`, f.klaxID)
			case "/api/reorder":
				body = fmt.Sprintf(`{"tabs":[%q,%q]}`, second.KlaxID, f.klaxID)
			}
			w := f.request(c.path, body, c.token)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("failed save: %d %s", w.Code, w.Body.String())
			}
			errorResponse(t, w, c.code)
			after, _ := json.Marshal(f.d.store.SessionsFor("user:test"))
			if !bytes.Equal(before, after) {
				t.Fatalf("failed save changed sessions: %s -> %s", before, after)
			}
			if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
				t.Fatal("failed mutation removed session files", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".before", path); err != nil {
				t.Fatal(err)
			}
			w = f.request(c.path, body, c.token)
			if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
				t.Fatalf("retry: %d %s", w.Code, w.Body.String())
			}
			reloaded, err := session.LoadStore()
			if err != nil {
				t.Fatal(err)
			}
			live, _ := json.Marshal(f.d.store.SessionsFor("user:test"))
			disk, _ := json.Marshal(reloaded.SessionsFor("user:test"))
			if !bytes.Equal(live, disk) || bytes.Equal(live, before) {
				t.Fatalf("retry was not durable: live=%s disk=%s prior=%s", live, disk, before)
			}
		})
	}
}

func TestAPIMultipartRejectsUnknownAndDuplicateFields(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	for _, field := range []string{"session", "klax_id", "upload"} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("klax_id", f.klaxID)
		_ = mw.WriteField("text", "hello")
		if field == "upload" {
			part, err := mw.CreateFormFile(field, "note.txt")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(part, "contents")
		} else {
			_ = mw.WriteField(field, "wrong")
		}
		_ = mw.Close()
		r := httptest.NewRequest("POST", "/api/send", &body)
		r.Header.Set("Authorization", "Bearer access")
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		f.s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", field, w.Code, w.Body.String())
		}
		errorResponse(t, w, "bad-request")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "backend.entered")); !os.IsNotExist(err) {
		t.Fatal("invalid form started a backend", err)
	}
}

func TestAPISettingsOmittedAndEmptyFields(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	f.d.store.UpdateSession("user:test", f.klaxID, func(cur *session.Session) {
		cur.ModelRequested, cur.Think, cur.SystemPrompt = "retained-model", "high", "prompt"
		cur.Groups = []string{"group"}
	})
	w := f.request("/api/settings", fmt.Sprintf(`{"klax_id":%q,"groups":[null]}`, f.klaxID), "")
	if w.Code != http.StatusBadRequest {
		t.Fatal(w.Code, w.Body.String())
	}
	cur := f.d.store.Get("user:test", f.klaxID)
	if len(cur.Groups) != 1 || cur.Groups[0] != "group" {
		t.Fatal("rejected groups changed", cur.Groups)
	}
	w = f.request("/api/settings", fmt.Sprintf(`{"klax_id":%q,"name":"renamed"}`, f.klaxID), "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	cur = f.d.store.Get("user:test", f.klaxID)
	if cur.ModelRequested != "retained-model" || cur.Think != "high" || cur.SystemPrompt != "prompt" || len(cur.Groups) != 1 {
		t.Fatal("omitted fields changed", cur)
	}
	w = f.request("/api/settings", fmt.Sprintf(`{"klax_id":%q,"model_requested":"","think":"","system_prompt":"","groups":[]}`, f.klaxID), "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	reloaded, err := session.LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	cur = reloaded.Get("user:test", f.klaxID)
	if cur.Name != "renamed" || cur.ModelRequested != "" || cur.Think != "" || cur.SystemPrompt != "" || len(cur.Groups) != 0 {
		t.Fatal("empty overrides not persisted", cur)
	}
}

func TestAPISettingsConflictsHaveDistinctCodes(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	f.d.store.UpdateSession("user:test", f.klaxID, func(cur *session.Session) { cur.Messages = 1 })
	for _, c := range []struct{ fields, code string }{
		{`"backend":"claude"`, "backend-locked"},
		{fmt.Sprintf(`"cwd":%q`, f.dir), "cwd-locked"},
	} {
		w := f.request("/api/settings", fmt.Sprintf(`{"klax_id":%q,%s}`, f.klaxID, c.fields), "")
		if w.Code != http.StatusConflict {
			t.Fatal(w.Code, w.Body.String())
		}
		errorResponse(t, w, c.code)
	}
	sr := f.d.getRunner("user:test", f.klaxID)
	sr.mu.Lock()
	sr.processing = true
	sr.mu.Unlock()
	w := f.request("/api/settings", fmt.Sprintf(`{"klax_id":%q,"think":""}`, f.klaxID), "")
	sr.mu.Lock()
	sr.processing = false
	sr.mu.Unlock()
	if w.Code != http.StatusConflict {
		t.Fatal(w.Code, w.Body.String())
	}
	errorResponse(t, w, "session-busy")
}

func TestAPIDisconnectDoesNotCancelAndNoIntermediateResponse(t *testing.T) {
	f := newAPIFixture(t, "block", "", "")
	server := httptest.NewServer(f.s.routes())
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/send", strings.NewReader(fmt.Sprintf(`{"klax_id":%q,"text":"hello","return_on":"finish","nonce":"lost"}`, f.klaxID)))
	req.Header.Set("Authorization", "Bearer access")
	done := make(chan error, 1)
	go func() {
		res, err := server.Client().Do(req)
		if res != nil {
			res.Body.Close()
		}
		done <- err
	}()
	f.entered(t, "start")
	select {
	case err := <-done:
		t.Fatalf("response before boundary: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected disconnect")
		}
	case <-time.After(time.Second):
		t.Fatal("client blocked")
	}
	f.release(t, "start")
	eventResponse(t, f.send("finish", "lost"), "finish", "success")
	eventResponse(t, f.send("finish", "next"), "finish", "success")
}

func TestAPIMultipartWait(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("klax_id", f.klaxID)
	_ = mw.WriteField("text", "read file")
	_ = mw.WriteField("return_on", "finish")
	part, _ := mw.CreateFormFile("files", "note.txt")
	_, _ = io.WriteString(part, "contents")
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/api/send", &body)
	r.Header.Set("Authorization", "Bearer access")
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	f.s.handleSend(w, r)
	ev := eventResponse(t, w, "finish", "success")
	if len(ev.Turn.Request.Attachments) != 1 || ev.Turn.Request.Attachments[0].Name != "note.txt" {
		t.Fatal("missing attachment snapshot")
	}
}

func TestAPINewRejectsInvalidWithoutPartialSession(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	for _, body := range []string{`{"backend":"unknown"}`, `{"cwd":"/nonexistent/klax-api-test"}`} {
		w := f.request("/api/new", body, "")
		if w.Code != 400 {
			t.Fatalf("%s: %d", body, w.Code)
		}
		if len(f.d.store.SessionsFor("user:test")) != 1 {
			t.Fatal("partial session created")
		}
	}
	if w := f.request("/api/new", `{"name":"ordinary"}`, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}

	// A non-directory parent makes persistence fail before publishing the session.
	invalid := filepath.Join(f.dir, "unwritable")
	if err := os.WriteFile(invalid, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KLAX_DATA_DIR", filepath.Join(invalid, "data"))
	broken, err := session.LoadStore()
	if err == nil {
		t.Fatal("expected inaccessible store", broken)
	}
	// Keep a loaded store path, then make its parent impossible to create.
	t.Setenv("KLAX_DATA_DIR", filepath.Join(f.dir, "store-parent", "data"))
	broken, err = session.LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	f.d.store = broken
	if err := os.WriteFile(filepath.Join(f.dir, "store-parent"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	w := f.request("/api/new", `{"name":"must-not-exist"}`, "")
	if w.Code != 500 || len(f.d.store.SessionsFor("user:test")) != 0 {
		t.Fatal("failed save published a session", w.Code)
	}
}

func TestAPIPreparationFailuresWakeBothBoundaries(t *testing.T) {
	for _, missingFiles := range []bool{false, true} {
		t.Run(fmt.Sprint(missingFiles), func(t *testing.T) {
			f := newAPIFixture(t, "", "", "")
			sr := f.d.getRunner("user:test", f.klaxID)
			sr.mu.Lock()
			sr.processing = true
			sr.mu.Unlock()
			admission := &sendAdmission{}
			var files []attachment
			if missingFiles {
				files = []attachment{{filename: "missing.txt", data: []byte("data")}}
			}
			if !f.d.handleInbound(Inbound{ChatID: "ui:test", Text: "hello", TargetKlaxID: f.klaxID, Nonce: "n", Attachments: files, RawMessage: true, admission: admission}) {
				t.Fatal("enqueue rejected")
			}
			sr.mu.Lock()
			msg := sr.queue[0]
			sr.queue = nil
			sr.processing = false
			sr.mu.Unlock()
			code := turnErrRunStartFailed
			if missingFiles {
				if err := os.Remove(sr.store.Path(msg.files[0])); err != nil {
					t.Fatal(err)
				}
				code = turnErrAttachmentsMissing
			} else {
				sr.store.Remove()
			}
			f.d.runBackend(msg)
			for _, boundary := range []string{"start", "finish"} {
				w := httptest.NewRecorder()
				awaitTurn(w, httptest.NewRequest("POST", "/api/send", nil), admission.completion, boundary)
				errorResponse(t, w, code)
			}
		})
	}
}

func TestAPIConcurrentDuplicateAcceptance(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	waits := make([]<-chan *httptest.ResponseRecorder, 16)
	for i := range waits {
		waits[i] = asyncResponse(func() *httptest.ResponseRecorder { return f.send("finish", "concurrent") })
	}
	id := ""
	for _, ch := range waits {
		event := eventResponse(t, response(t, ch), "finish", "success")
		if id != "" && id != event.Turn.ID {
			t.Fatal("duplicates bound to different turns")
		}
		id = event.Turn.ID
	}
	runs, _ := os.ReadFile(filepath.Join(f.dir, "runs"))
	if string(runs) != "x\n" {
		t.Fatalf("duplicate executions: %q", runs)
	}
}

type delayedAPIWriter struct {
	header  http.Header
	writing chan struct{}
	release chan struct{}
}

func (w *delayedAPIWriter) Header() http.Header { return w.header }
func (w *delayedAPIWriter) WriteHeader(int)     {}
func (w *delayedAPIWriter) Write(p []byte) (int, error) {
	close(w.writing)
	<-w.release
	return 0, io.ErrClosedPipe
}

func TestAPISlowFailedWriterDoesNotBlockQueue(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	w := &delayedAPIWriter{header: make(http.Header), writing: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest("POST", "/api/send", strings.NewReader(fmt.Sprintf(`{"klax_id":%q,"text":"hello","return_on":"finish","nonce":"slow"}`, f.klaxID)))
	req.Header.Set("Authorization", "Bearer access")
	done := make(chan struct{})
	go func() { defer close(done); f.s.handleSend(w, req) }()
	defer func() {
		close(w.release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("failed writer did not exit")
		}
	}()
	select {
	case <-w.writing:
	case <-time.After(5 * time.Second):
		t.Fatal("writer never reached")
	}
	eventResponse(t, f.send("finish", "next"), "finish", "success")
}

func TestAPIDeletionReleasesActiveGateWaiter(t *testing.T) {
	f := newAPIFixture(t, "block", "", "")
	f.d.store.New("user:test", "other", f.dir, session.ScopeDefaults{Backend: "codex"})
	wait := asyncResponse(func() *httptest.ResponseRecorder { return f.send("finish", "deleted") })
	f.entered(t, "start")
	w := f.request("/api/close", fmt.Sprintf(`{"klax_id":%q}`, f.klaxID), "")
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	errorResponse(t, response(t, wait), "session-deleted")
	f.release(t, "start")
	f.d.drainWg.Wait()
}

func TestAPIFinalSaveFailure(t *testing.T) {
	f := newAPIFixture(t, "", "", "block")
	wait := asyncResponse(func() *httptest.ResponseRecorder { return f.send("finish", "save") })
	f.entered(t, "backend")
	dataDir := filepath.Join(f.dir, "data")
	if err := os.Rename(dataDir, filepath.Join(f.dir, "saved-data")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f.release(t, "backend")
	errorResponse(t, response(t, wait), "result-save-failed")
}

func TestMessengerDeletionPreservesFilesOnSaveFailure(t *testing.T) {
	for _, command := range []string{"cleanup", "nuke"} {
		t.Run(command, func(t *testing.T) {
			f := newAPIFixture(t, "", "", "")
			const sk = "user:test"
			f.d.store.New(sk, "active", f.dir, session.ScopeDefaults{Backend: "codex"})
			if err := f.d.store.Save(); err != nil {
				t.Fatal(err)
			}
			sr := f.d.getRunner(sk, f.klaxID)
			cancelled := false
			sr.cancel = func() { cancelled = true }
			_, _, files, _, err := sr.store.Enqueue("tg:1", "", "pending", "pending", []sessfiles.NamedReader{{Name: "report.txt", R: strings.NewReader("DATA")}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(session.StoreDir(), "sessions.json")
			if err := os.Rename(path, path+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if command == "cleanup" {
				f.d.handleSessionDelete("tg:1", "", sk, "1")
			} else if deleted, aborted, err := f.d.deleteInactiveSessions(sk); err == nil || deleted != 0 || aborted != 0 {
				t.Fatalf("failed deletion = %d, %d, %v", deleted, aborted, err)
			}
			if f.d.store.Get(sk, f.klaxID) == nil || cancelled || f.d.lookupRunner(sk, f.klaxID) != sr {
				t.Fatal("failed deletion removed or aborted the session")
			}
			data, err := os.ReadFile(sr.store.Path(files[0]))
			if err != nil || string(data) != "DATA" {
				t.Fatalf("file destroyed: %q, %v", data, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".saved", path); err != nil {
				t.Fatal(err)
			}
			loaded, err := session.LoadStore()
			if err != nil || loaded.Get(sk, f.klaxID) == nil {
				t.Fatalf("persisted session lost: %v", err)
			}
		})
	}
}

func TestEmptySessionBackendStaysResolvedAcrossTurns(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	f.d.cfg.DefaultBackend = "claude"
	f.d.store.UpdateSession("user:test", f.klaxID, func(sess *session.Session) { sess.Backend = "" })
	f.d.store.UpdateScopeDefaults("user:test", func(def *session.ScopeDefaults) { def.Backend = "codex" })
	settings, _ := f.d.uiSessionSettings("user:test", f.klaxID)
	tabs := f.d.sessionsSnapshot("user:test", f.d.store.SessionsFor("user:test"), nil, false)
	if settings.Backend != "codex" || tabs[0].Backend != "codex" {
		t.Fatalf("settings and tabs disagree: %s, %s", settings.Backend, tabs[0].Backend)
	}
	for _, nonce := range []string{"first", "second"} {
		w := f.send("finish", nonce)
		eventResponse(t, w, "finish", "success")
	}
	loaded, err := session.LoadStore()
	if err != nil || loaded.Get("user:test", f.klaxID).Backend != "codex" {
		t.Fatalf("backend not persisted: %v", err)
	}
	runs, err := os.ReadFile(filepath.Join(f.dir, "runs"))
	if err != nil || strings.Count(string(runs), "x") != 2 {
		t.Fatalf("both turns did not use the selected backend: %q, %v", runs, err)
	}
}

func TestSettingsEmptyListsAndRenameErrors(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	f.d.models = nil
	for _, query := range []string{"", "?klax_id=" + f.klaxID} {
		r := httptest.NewRequest(http.MethodGet, "/api/settings"+query, nil)
		r.Header.Set("Authorization", "Bearer access")
		w := httptest.NewRecorder()
		f.s.routes().ServeHTTP(w, r)
		var data map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil || w.Code != http.StatusOK {
			t.Fatalf("settings: %d %s, %v", w.Code, w.Body.String(), err)
		}
		for _, field := range []string{"models", "groups"} {
			if string(data[field]) != "[]" {
				t.Fatalf("%s = %s, want []", field, data[field])
			}
		}
	}
	for _, path := range []string{"/api/rename", "/api/settings"} {
		w := f.request(path, fmt.Sprintf(`{"klax_id":%q,"name":""}`, f.klaxID), "")
		errorResponse(t, w, "invalid-settings")
	}
}

func TestAPIResultRetentionPreservesPendingWaiters(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	sr := f.d.getRunner("user:test", f.klaxID)
	sr.mu.Lock()
	sr.results = make(map[int64]*turnWait)
	pending := newTurnWait()
	sr.results[1] = pending
	for i := int64(2); i <= retainedTurnResults+3; i++ {
		r := newTurnWait()
		r.fail("aborted")
		sr.results[i] = r
	}
	attached := sr.results[2]
	sr.pruneResultsLocked()
	if sr.results[1] != pending || sr.results[2] != nil || len(sr.results) != retainedTurnResults {
		t.Fatal("retention discarded a pending turn or retained excess results")
	}
	sr.mu.Unlock()
	w := httptest.NewRecorder()
	awaitTurn(w, httptest.NewRequest("POST", "/api/send", nil), attached, "finish")
	errorResponse(t, w, "aborted")
}

func TestMessengerSessionControl(t *testing.T) {
	for _, chatID := range []string{"tg:1", "mx:1", "vk:1", "ym:test@example.org"} {
		t.Run(chatID, func(t *testing.T) {
			f := newAPIFixture(t, "", "", "block")
			f.d.identities = map[int64]string{1: "test"}
			f.d.maxIdents = map[int64]string{1: "test"}
			f.d.vkIdents = map[int]string{1: "test"}
			f.d.ymIdents = map[string]string{"test@example.org": "test"}
			if w := f.request("/api/new", `{"name":"managed"}`, ""); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			id := f.d.store.Active("user:test").KlaxID
			f.d.handleCommand(chatID, "", "/name renamed")
			f.d.handleCommand(chatID, "", "/prompt instructions")
			got := f.d.store.Get("user:test", id)
			if got.Name != "renamed" || got.SystemPrompt != "instructions" {
				t.Fatal("messenger settings blocked")
			}
			if !f.d.handleInbound(Inbound{ChatID: chatID, MsgID: "1", Text: "hello", Attachments: []attachment{{filename: "note.txt", data: []byte("note")}}}) {
				t.Fatal("messenger send blocked")
			}
			f.entered(t, "backend")
			sr := f.d.getRunner("user:test", id)
			sr.mu.Lock()
			var completion *turnWait
			for _, result := range sr.results {
				completion = result
			}
			sr.mu.Unlock()
			if completion == nil {
				t.Fatal("missing accepted turn")
			}
			f.d.handleCommand(chatID, "", "/abort")
			w := httptest.NewRecorder()
			awaitTurn(w, httptest.NewRequest("POST", "/api/send", nil), completion, "finish")
			eventResponse(t, w, "finish", "aborted")
			until := time.Now().Add(5 * time.Second)
			for f.d.isSessionBusy("user:test", id) {
				if time.Now().After(until) {
					t.Fatal("session remained busy after abort")
				}
				time.Sleep(time.Millisecond)
			}
			f.d.store.Switch("user:test", 0)
			f.d.handleSessionDelete(chatID, "", "user:test", "2")
			if f.d.store.Get("user:test", id) != nil {
				t.Fatal("messenger deletion blocked")
			}
		})
	}
}

func TestMessengerNukeWakesAPI(t *testing.T) {
	f := newAPIFixture(t, "block", "", "")
	f.d.identities = map[int64]string{1: "test"}
	if w := f.request("/api/new", `{"name":"managed"}`, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.d.store.UpdateScopeDefaults("user:test", func(def *session.ScopeDefaults) { def.GroupAttachmentMode = "any" })
	id := f.d.store.Active("user:test").KlaxID
	wait := asyncResponse(func() *httptest.ResponseRecorder {
		return f.request("/api/send", fmt.Sprintf(`{"klax_id":%q,"text":"hello","return_on":"finish"}`, id), "")
	})
	f.entered(t, "start")
	f.d.handleCommand("tg:1", "", "/nuke fresh")
	errorResponse(t, response(t, wait), "session-deleted")
	got := f.d.store.SessionsFor("user:test")
	if len(got) != 1 || got[0].Name != "fresh" || !got[0].Active {
		t.Fatal("nuke did not replace all sessions")
	}
	if f.d.scopeDefaults("user:test").GroupAttachmentMode != "any" {
		t.Fatal("nuke reset the scope's attachment mode")
	}
	if got := f.d.scopeDefaults("user:test").CWD; got != "" {
		t.Fatalf("nuke pinned the configured cwd: %q", got)
	}
	nextCWD := t.TempDir()
	f.d.cfg.DefaultCWD = nextCWD
	if next, _ := f.d.createSession("tg:1", "user:test", "next"); next.CWD != nextCWD {
		t.Fatalf("new session cwd = %q, want changed config %q", next.CWD, nextCWD)
	}
	f.release(t, "start")
}
