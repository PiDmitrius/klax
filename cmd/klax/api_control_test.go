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
	"github.com/PiDmitrius/klax/internal/session"
	"github.com/PiDmitrius/klax/internal/turnaudit"
)

type apiFixture struct {
	d       *daemon
	s       *uiServer
	dir     string
	created int64
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
			script += "touch \"$KLAX_API_TEST_DIR/" + phase + ".entered\"\nwhile [ ! -f \"$KLAX_API_TEST_DIR/" + phase + ".release\" ]; do sleep 0.01; done\n"
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
		script += "while [ ! -f \"$KLAX_API_TEST_DIR/backend.release\" ]; do sleep 0.01; done\n"
	}
	if backend == "fail" {
		script += "exit 1\n"
	} else {
		script += "printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}'\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	sess := d.store.New("user:test", "test", dir, session.ScopeDefaults{Backend: "codex"})
	f := &apiFixture{d: d, s: &uiServer{d: d, tokens: map[string]uiAccess{"access": {User: "test"}, "other": {User: "other"}}}, dir: dir, created: sess.Created}
	t.Cleanup(func() {
		for _, phase := range []string{"start", "finish", "backend"} {
			_ = os.WriteFile(filepath.Join(dir, phase+".release"), nil, 0600)
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
	return f.request("/api/send", fmt.Sprintf(`{"session":%d,"text":"hello","return_on":%q,"nonce":%q}`, f.created, boundary, nonce), "")
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
		w := f.request("/api/send", fmt.Sprintf(`{"session":%d,"text":"hello","return_on":"finish"}`, f.created), "")
		ev := eventResponse(t, w, "finish", "success")
		if ev.Turn.Result.Output.Text != "done" {
			t.Fatal("missing backend output")
		}
	}
	turns, err := f.d.getRunner("user:test", f.created).store.InboundLog()
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
				f.d.abortSession("user:test", f.created, false)
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
			f.d.abortSession("user:test", f.created, closing)
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
	sr := f.d.getRunner("user:test", f.created)
	if _, _, _, _, err := sr.store.Enqueue("ui:test", "", "historical", "hello", nil); err != nil {
		t.Fatal(err)
	}
	if w := f.send("queued", "historical"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	errorResponse(t, f.send("finish", "historical"), "result-unavailable")
	for _, fields := range []string{`"nonce":""`, `"nonce":null`, `"nonce":1`, `"return_on":""`, `"return_on":true`, `"return_on":"later"`} {
		w := f.request("/api/send", fmt.Sprintf(`{"session":%d,"text":"hello",%s}`, f.created, fields), "")
		if w.Code != 400 {
			t.Fatalf("%s: %d", fields, w.Code)
		}
	}
	turns, _ := sr.store.InboundLog()
	if len(turns) != 1 {
		t.Fatal("invalid request queued")
	}
}

func TestAPIDisconnectDoesNotCancelAndNoIntermediateResponse(t *testing.T) {
	f := newAPIFixture(t, "block", "", "")
	server := httptest.NewServer(f.s.routes())
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/send", strings.NewReader(fmt.Sprintf(`{"session":%d,"text":"hello","return_on":"finish","nonce":"lost"}`, f.created)))
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
	_ = mw.WriteField("session", fmt.Sprint(f.created))
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
	for _, body := range []string{`{"control_token":""}`, `{"control_token":null}`, `{"control_token":false}`, `{"backend":"unknown"}`, `{"cwd":"/nonexistent/klax-api-test"}`} {
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
			sr := f.d.getRunner("user:test", f.created)
			sr.mu.Lock()
			sr.processing = true
			sr.mu.Unlock()
			admission := &sendAdmission{}
			var files []attachment
			if missingFiles {
				files = []attachment{{filename: "missing.txt", data: []byte("data")}}
			}
			if !f.d.handleInbound(Inbound{ChatID: "ui:test", Text: "hello", TargetCreated: f.created, Nonce: "n", Attachments: files, RawMessage: true, admission: admission}) {
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
	req := httptest.NewRequest("POST", "/api/send", strings.NewReader(fmt.Sprintf(`{"session":%d,"text":"hello","return_on":"finish","nonce":"slow"}`, f.created)))
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
	w := f.request("/api/close", fmt.Sprintf(`{"session":%d}`, f.created), "")
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

func TestAPIResultRetentionPreservesPendingWaiters(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	sr := f.d.getRunner("user:test", f.created)
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
			id := f.d.store.Active("user:test").Created
			f.d.handleCommand(chatID, "", "/name renamed")
			f.d.handleCommand(chatID, "", "/prompt instructions")
			got := f.d.store.Get("user:test", id)
			if got.Name != "renamed" || got.AppendSystemPrompt != "instructions" {
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
	id := f.d.store.Active("user:test").Created
	wait := asyncResponse(func() *httptest.ResponseRecorder {
		return f.request("/api/send", fmt.Sprintf(`{"session":%d,"text":"hello","return_on":"finish"}`, id), "")
	})
	f.entered(t, "start")
	f.d.handleCommand("tg:1", "", "/nuke fresh")
	errorResponse(t, response(t, wait), "session-deleted")
	got := f.d.store.SessionsFor("user:test")
	if len(got) != 1 || got[0].Name != "fresh" || !got[0].Active {
		t.Fatal("nuke did not replace all sessions")
	}
	f.release(t, "start")
}
