package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/session"
)

func readerFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPIFixture(t, "capture", "capture", "")
	f.d.uiHub = newUIHub()
	var err error
	f.s.tokens, err = buildUITokens([]config.UserIdentity{{ID: "test", UIToken: "access", UIReadToken: "reader"}, {ID: "other", UIToken: "other", UIReadToken: "other-reader"}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func accessGet(f *apiFixture, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, r)
	return w
}

func TestReadTokenPermissionsAndIdentity(t *testing.T) {
	f := readerFixture(t)
	body := fmt.Sprintf(`{"session":%d,"name":"changed","text":"hello","order":[%d]}`, f.created, f.created)
	for _, path := range []string{"/api/new", "/api/send", "/api/abort", "/api/rename", "/api/reorder", "/api/close", "/api/settings", "/api/system/check", "/api/system/update"} {
		w := f.request(path, body, "reader")
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		errorResponse(t, w, "read-only")
	}
	for _, path := range []string{"/api/send", "/api/abort", "/api/rename", "/api/close", "/api/settings"} {
		if w := f.request(path, body, "other"); w.Code != 404 {
			t.Fatalf("cross-user %s: %d", path, w.Code)
		}
	}
	if got := f.d.store.SessionsFor("user:test"); len(got) != 1 || got[0].Name != "test" {
		t.Fatal("reader mutated sessions")
	}
	if w := f.request("/api/send", body, "invalid"); w.Code != 401 {
		t.Fatal("invalid token accepted")
	}
	for _, path := range []string{"/api/auth", "/api/sessions", fmt.Sprintf("/api/settings?session=%d", f.created), fmt.Sprintf("/api/transcript?session=%d", f.created)} {
		w := accessGet(f, path, "reader")
		if w.Code != 200 {
			t.Fatalf("read %s: %d %s", path, w.Code, w.Body.String())
		}
		if !strings.Contains(path, "transcript") && !strings.Contains(w.Body.String(), `"read_only":true`) {
			t.Fatalf("missing role %s", path)
		}
		if strings.Contains(w.Body.String(), `"reader"`) || strings.Contains(w.Body.String(), `"access"`) {
			t.Fatal("credential exposed")
		}
	}
	if w := accessGet(f, fmt.Sprintf("/api/transcript?session=%d", f.created), "other-reader"); w.Code != 404 {
		t.Fatal("cross-user read accepted", w.Code)
	}
	if w := f.request("/api/read", fmt.Sprintf(`{"session":%d,"turn":1,"block":0}`, f.created), "other-reader"); w.Code != 404 {
		t.Fatal("cross-user mark accepted", w.Code)
	}
	eventResponse(t, f.send("finish", "test-turn"), "finish", "success")
	for _, name := range []string{"prompt", "args", "start.json", "finish.json"} {
		data, err := os.ReadFile(filepath.Join(f.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "reader") || strings.Contains(string(data), "Bearer access") {
			t.Fatal("credential reached execution")
		}
	}
}

func TestReaderWatermarksAreIndependentAndDurable(t *testing.T) {
	f := readerFixture(t)
	f.d.cfg.Audit.Turn.Finish.Command = []string{"/bin/sh", "-c", "exit 1"}
	eventResponse(t, f.send("finish", "test-turn"), "finish", "success")
	mark := func(token string, turn int64, block int) {
		t.Helper()
		w := f.request("/api/read", fmt.Sprintf(`{"session":%d,"turn":%d,"block":%d}`, f.created, turn, block), token)
		if w.Code != 204 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	snapshot := func(token string) uiSessionInfo {
		t.Helper()
		w := accessGet(f, "/api/sessions", token)
		var list []uiSessionInfo
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 {
			t.Fatal("snapshot", err, w.Body.String())
		}
		return list[0]
	}
	before := snapshot("access")
	mark("reader", 1, 0)
	owner, reader := snapshot("access"), snapshot("reader")
	if owner.ReadThrough != "0.0" || owner.Unread != before.Unread || reader.ReadThrough != "1.0" || reader.Unread >= owner.Unread {
		t.Fatalf("mixed markers: owner=%+v reader=%+v", owner, reader)
	}
	mark("access", 2, 3)
	mark("reader", 0, 0)
	if snapshot("reader").ReadThrough != "1.0" || snapshot("access").ReadThrough != "2.3" {
		t.Fatal("watermark crossed roles or regressed")
	}
	f.d.store.UpdateSession("user:test", f.created, func(cur *session.Session) { cur.Name = "renamed" })
	if err := f.d.store.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := session.LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	f.d.store = reloaded
	delete(f.s.tokens, "reader")
	f.s.tokens["rotated-reader"] = uiAccess{User: "test", ReadOnly: true}
	if snapshot("rotated-reader").ReadThrough != "1.0" || snapshot("access").ReadThrough != "2.3" {
		t.Fatal("markers lost after reload/rotation")
	}
	transcript := accessGet(f, fmt.Sprintf("/api/transcript?session=%d", f.created), "rotated-reader")
	if !strings.Contains(transcript.Body.String(), `"read_through":"1.0"`) {
		t.Fatal("transcript uses wrong reader")
	}
	w := f.request("/api/tail", `{"cursors":{},"sess_rev":999999}`, "rotated-reader")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"read_through":"1.0"`) {
		t.Fatal("tail uses wrong reader", w.Code, w.Body.String())
	}
}

func TestReaderDoesNotCreateInitialSession(t *testing.T) {
	f := readerFixture(t)
	for _, method := range []string{"GET", "POST"} {
		var w *httptest.ResponseRecorder
		if method == "GET" {
			w = accessGet(f, "/api/sessions", "other-reader")
		} else {
			w = f.request("/api/tail", `{"cursors":{},"sess_rev":999999}`, "other-reader")
		}
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if len(f.d.store.SessionsFor("user:other")) != 0 {
			t.Fatal("reading created session")
		}
	}
}

func TestAccessTokenCollisions(t *testing.T) {
	if !hasConfiguredTransport(&config.Config{UIListen: "127.0.0.1:0", Users: []config.UserIdentity{{ID: "viewer", UIReadToken: "reader"}}}) {
		t.Fatal("read-only UI cannot start alone")
	}
	for _, users := range [][]config.UserIdentity{
		{{ID: "test", UIToken: "same", UIReadToken: "same"}},
		{{ID: "one", UIToken: "same"}, {ID: "two", UIReadToken: "same"}},
		{{ID: "one", UIReadToken: "same"}, {ID: "two", UIReadToken: "same"}},
		{{UIReadToken: "same"}},
	} {
		if _, err := buildUITokens(users); err == nil || strings.Contains(err.Error(), "same") {
			t.Fatal("invalid credentials accepted or exposed")
		}
	}
}

func TestNewSessionSettingsWithManagementAccess(t *testing.T) {
	f := readerFixture(t)
	body := fmt.Sprintf(`{"name":"configured","cwd":%q,"backend":"codex","model":"gpt-5.6-sol","think":"high","sandbox":"on","prompt":"instructions","groups":["work"]}`, f.dir)
	w := f.request("/api/new", body, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	reloaded, err := session.LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Active("user:test")
	if got.Name != "configured" || got.CWD != f.dir || got.Backend != "codex" || got.ModelOverride != "gpt-5.6-sol" || got.ThinkOverride != "high" || got.Sandbox != "on" || got.AppendSystemPrompt != "instructions" || len(got.Groups) != 1 || got.Groups[0] != "work" {
		t.Fatal("settings lost during creation")
	}
	req := httptest.NewRequest("POST", "/api/send", strings.NewReader(fmt.Sprintf(`{"session":%d,"text":"hello"}`, got.Created)))
	req.Header.Set("Authorization", "Bearer reader")
	req.Header.Set("X-Klax-Control-Token", "anything")
	rec := httptest.NewRecorder()
	f.s.routes().ServeHTTP(rec, req)
	errorResponse(t, rec, "read-only")
}
