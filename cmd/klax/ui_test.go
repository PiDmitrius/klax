package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/config"
)

// A slow SSE client must be dropped, never block the broadcaster (which runs in
// the runner's stdout goroutine).
func TestUIHubBroadcastDropsSlowClient(t *testing.T) {
	h := newUIHub()
	c := h.subscribe("claw")
	for i := 0; i < uiClientBuffer+5; i++ {
		h.broadcast("claw", []byte("x")) // never drained
	}
	h.mu.Lock()
	_, present := h.clients[c]
	n := len(h.clients)
	h.mu.Unlock()
	if present || n != 0 {
		t.Fatalf("slow client not dropped: present=%v remaining=%d", present, n)
	}
}

func TestUIHubBroadcastIsolatesUsers(t *testing.T) {
	h := newUIHub()
	a := h.subscribe("alice")
	b := h.subscribe("bob")
	h.broadcast("alice", []byte("hi"))
	select {
	case <-a.ch:
	default:
		t.Fatal("alice did not receive her own event")
	}
	select {
	case <-b.ch:
		t.Fatal("bob received alice's event")
	default:
	}
}

func TestBuildUITokens(t *testing.T) {
	tokens, err := buildUITokens([]config.UserIdentity{
		{ID: "claw", UIToken: "secret1"},
		{ID: "bob", UIToken: "secret2"},
		{ID: "noui"}, // no token — skipped, not an error
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 2 || tokens["secret1"] != "claw" || tokens["secret2"] != "bob" {
		t.Fatalf("bad token map: %v", tokens)
	}
	if _, err := buildUITokens([]config.UserIdentity{{ID: "a", UIToken: "dup"}, {ID: "b", UIToken: "dup"}}); err == nil {
		t.Fatal("duplicate ui_token must be rejected")
	}
	if _, err := buildUITokens([]config.UserIdentity{{ID: "", UIToken: "x"}}); err == nil {
		t.Fatal("empty id with a ui_token must be rejected")
	}
}

func TestUIServerAuth(t *testing.T) {
	s := &uiServer{tokens: map[string]string{"secret": "claw"}}

	bearer := httptest.NewRequest("GET", "/api/sessions", nil)
	bearer.Header.Set("Authorization", "Bearer secret")
	if u, ok := s.auth(bearer); !ok || u != "claw" {
		t.Fatalf("bearer auth: got %q ok=%v", u, ok)
	}

	// A query-string token authenticates ONLY via the SSE-specific path...
	query := httptest.NewRequest("GET", "/api/events?token=secret", nil)
	if u, ok := s.authSSE(query); !ok || u != "claw" {
		t.Fatalf("SSE query auth: got %q ok=%v", u, ok)
	}
	// ...and is rejected by the Bearer-only auth used on every other route.
	if _, ok := s.auth(query); ok {
		t.Fatal("query token must not authenticate non-SSE routes")
	}

	bad := httptest.NewRequest("GET", "/api/sessions", nil)
	bad.Header.Set("Authorization", "Bearer nope")
	if _, ok := s.auth(bad); ok {
		t.Fatal("unknown token must not authenticate")
	}

	none := httptest.NewRequest("GET", "/api/sessions", nil)
	if _, ok := s.auth(none); ok {
		t.Fatal("missing token must not authenticate")
	}
}

// The UI shares the session list with messenger DMs for the same person: a
// ui:<id> chat and a mapped tg DM both resolve to user:<id>.
func TestSessionKeyUISharesIdentity(t *testing.T) {
	d := &daemon{identities: map[int64]string{42: "claw"}}
	if got := d.sessionKey("ui:claw"); got != "user:claw" {
		t.Fatalf("ui sessionKey = %q, want user:claw", got)
	}
	if got := d.sessionKey("tg:42"); got != "user:claw" {
		t.Fatalf("tg sessionKey = %q, want user:claw (shared identity)", got)
	}
}

func TestDeliveryForRoutesUIChat(t *testing.T) {
	d := &daemon{uiHub: newUIHub()}
	del := d.deliveryFor(context.Background(), queuedMsg{chatID: "ui:claw", sessCreated: 5}, true)
	if _, ok := del.(*uiDelivery); !ok {
		t.Fatalf("ui chat must get *uiDelivery, got %T", del)
	}
	del.Close()
}

// End-to-end through the real mux: the SPA is served, the API rejects requests
// without a token, and a valid token gets the session snapshot for that user.
func TestUIServerRoutes(t *testing.T) {
	d := &daemon{
		cfg:     &config.Config{},
		store:   newStoreWithChat("user:claw", "one"),
		uiHub:   newUIHub(),
		runners: make(map[runnerKey]*sessionRunner),
	}
	h := (&uiServer{d: d, tokens: map[string]string{"sec": "claw"}}).routes()

	spa := httptest.NewRecorder()
	h.ServeHTTP(spa, httptest.NewRequest("GET", "/", nil))
	if spa.Code != 200 || !strings.Contains(spa.Body.String(), "klax") {
		t.Fatalf("SPA: code=%d", spa.Code)
	}

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest("GET", "/api/sessions", nil))
	if unauth.Code != 401 {
		t.Fatalf("unauthenticated /api/sessions: code=%d, want 401", unauth.Code)
	}

	ok := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(ok, req)
	if ok.Code != 200 || !strings.Contains(ok.Body.String(), `"created"`) {
		t.Fatalf("authenticated /api/sessions: code=%d body=%s", ok.Code, ok.Body.String())
	}

	// The UI has no chat commands: the /api/command endpoint is gone (it falls
	// through to the SPA's not-found rather than dispatching a legacy command).
	cmd := httptest.NewRecorder()
	creq := httptest.NewRequest("POST", "/api/command", strings.NewReader(`{"text":"/model"}`))
	creq.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(cmd, creq)
	if cmd.Code != 404 {
		t.Fatalf("/api/command must be removed: code=%d, want 404", cmd.Code)
	}
}

func TestUISendRequiresSession(t *testing.T) {
	d := &daemon{cfg: &config.Config{}, store: newStoreWithChat("user:claw", "one"),
		uiHub: newUIHub(), runners: make(map[runnerKey]*sessionRunner)}
	h := (&uiServer{d: d, tokens: map[string]string{"sec": "claw"}}).routes()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/send", strings.NewReader(`{"text":"hi"}`))
	req.Header.Set("Authorization", "Bearer sec")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("/api/send without a session: code=%d, want 400 (must not hit the active session)", rec.Code)
	}
}

func TestUIAbortValidatesSession(t *testing.T) {
	d := &daemon{store: newStoreWithChat("user:claw", "one"), runners: make(map[runnerKey]*sessionRunner)}
	h := (&uiServer{d: d, tokens: map[string]string{"sec": "claw"}}).routes()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/abort", strings.NewReader(`{"session":99999}`))
	req.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("/api/abort on a missing session: code=%d, want 404", rec.Code)
	}
}

// The SPA's product name (browser tab title + login heading) comes from
// config.ui_title, injected server-side per request; empty falls back to "klax";
// the value is HTML-escaped.
func TestHandleSPAInjectsUITitle(t *testing.T) {
	serve := func(title string) string {
		s := &uiServer{d: &daemon{cfg: &config.Config{UITitle: title}}}
		rec := httptest.NewRecorder()
		s.handleSPA(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 {
			t.Fatalf("handleSPA code=%d", rec.Code)
		}
		return rec.Body.String()
	}

	custom := serve("KLODIN")
	if !strings.Contains(custom, "<title>KLODIN</title>") || !strings.Contains(custom, "<h2>KLODIN</h2>") {
		t.Fatalf("custom title not injected into <title>/<h2>")
	}
	if strings.Contains(custom, "__KLAX_UI_TITLE__") {
		t.Fatal("placeholder left unreplaced")
	}

	if dflt := serve(""); !strings.Contains(dflt, "<title>klax</title>") {
		t.Fatal("empty ui_title must default to klax")
	}

	if esc := serve("<b>"); strings.Contains(esc, "<title><b></title>") || !strings.Contains(esc, "&lt;b&gt;") {
		t.Fatal("ui_title must be HTML-escaped")
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8799": true, "localhost:8799": true, "[::1]:8799": true,
		":8799": false, "0.0.0.0:8799": false, "192.168.1.5:8799": false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
