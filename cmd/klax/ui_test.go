package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/config"
)

func decodeEvent(t *testing.T, raw json.RawMessage) uiEvent {
	t.Helper()
	var ev uiEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	return ev
}

// Every broadcast event gets a monotonic seq and is retained per user; collect
// returns everything after a cursor, the head, and whether the cursor is coverable.
func TestUIHubCollect(t *testing.T) {
	h := newUIHub()
	for i := 0; i < 5; i++ {
		h.broadcast("claw", uiEvent{Type: "progress", Text: "x"})
	}
	// From cursor 0: all 5, head 5, no reload.
	ev, head, reload := h.collect("claw", h.epoch, 0)
	if reload || head != 5 || len(ev) != 5 {
		t.Fatalf("collect(0): n=%d head=%d reload=%v, want 5/5/false", len(ev), head, reload)
	}
	if first := decodeEvent(t, ev[0]); first.Seq != 1 {
		t.Fatalf("first seq=%d, want 1", first.Seq)
	}
	// From cursor 2: exactly 3,4,5.
	if ev, _, _ := h.collect("claw", h.epoch, 2); len(ev) != 3 || decodeEvent(t, ev[0]).Seq != 3 {
		t.Fatalf("collect(2): n=%d first=%d, want 3 starting at seq 3", len(ev), decodeEvent(t, ev[0]).Seq)
	}
	// Up to date: nothing, no reload.
	if ev, _, reload := h.collect("claw", h.epoch, 5); reload || len(ev) != 0 {
		t.Fatalf("collect(5): n=%d reload=%v, want 0/false", len(ev), reload)
	}
	// Stale epoch (daemon restart) -> reload.
	if _, _, reload := h.collect("claw", h.epoch+1, 2); !reload {
		t.Fatal("a stale-epoch cursor must report reload")
	}
}

// A cursor older than the retained ring (overflow) -> reload (transcript backstop).
func TestUIHubCollectGapOnOverflow(t *testing.T) {
	h := newUIHub()
	for i := 0; i < uiRingMaxItems+50; i++ {
		h.broadcast("claw", uiEvent{Type: "progress", Text: "x"})
	}
	if _, _, reload := h.collect("claw", h.epoch, 1); !reload {
		t.Fatal("a cursor predating the ring must report reload")
	}
}

// Events are retained per user; one user's events never leak into another's.
func TestUIHubIsolatesUsers(t *testing.T) {
	h := newUIHub()
	h.broadcast("alice", uiEvent{Type: "notice", Text: "hi"})
	if ev, _, _ := h.collect("alice", h.epoch, 0); len(ev) != 1 {
		t.Fatalf("alice got %d events, want 1", len(ev))
	}
	if ev, _, _ := h.collect("bob", h.epoch, 0); len(ev) != 0 {
		t.Fatalf("bob got %d events, want 0 (not alice's)", len(ev))
	}
}

// A held poll grabs its user's wake channel before reading the ring; an emit for
// that user closes the channel it holds (lost-wakeup-safe).
func TestUIHubWakeOnEmit(t *testing.T) {
	h := newUIHub()
	ch := h.waitChan("claw")
	select {
	case <-ch:
		t.Fatal("wake channel fired before any emit")
	default:
	}
	h.broadcast("claw", uiEvent{Type: "progress", Text: "x"})
	select {
	case <-ch: // emit closed the channel we were holding
	default:
		t.Fatal("emit did not wake a held waiter")
	}
}

// Concurrently-held polls per user are capped (bounds goroutines without
// per-connection state); a freed slot allows a new poll.
func TestUIHubInflightCap(t *testing.T) {
	h := newUIHub()
	for i := 0; i < uiMaxInflightPerUser; i++ {
		if !h.enterPoll("claw") {
			t.Fatalf("enterPoll refused at %d, under the cap", i)
		}
	}
	if h.enterPoll("claw") {
		t.Fatal("enterPoll must refuse past the cap")
	}
	h.leavePoll("claw")
	if !h.enterPoll("claw") {
		t.Fatal("a freed slot must allow a new poll")
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

	// A query-string token is NOT accepted: every UI request (incl. the long-poll)
	// sets the Authorization header, so there is no ?token= auth path.
	query := httptest.NewRequest("GET", "/api/poll?token=secret", nil)
	if _, ok := s.auth(query); ok {
		t.Fatal("query token must not authenticate")
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

func TestUIDeliveryUsesCanonicalSessionUser(t *testing.T) {
	h := newUIHub()
	d := &daemon{uiHub: h}
	del := d.newUIDelivery(context.Background(), queuedMsg{
		chatID:      "tg:42",
		sessKey:     "user:claw",
		sessCreated: 7,
	})
	del.Close()

	ev, _, _ := h.collect("claw", h.epoch, 0)
	if len(ev) != 1 {
		t.Fatalf("canonical user got %d events, want 1 turn_start", len(ev))
	}
	if e := decodeEvent(t, ev[0]); e.Type != "turn_start" || e.Session != 7 {
		t.Fatalf("event = %+v, want turn_start for session 7", e)
	}
	if raw, _, _ := h.collect("42", h.epoch, 0); len(raw) != 0 {
		t.Fatal("raw messenger id received the canonical UI event")
	}
}

func TestUITransportUsesCanonicalSessionUser(t *testing.T) {
	h := newUIHub()
	d := &daemon{uiHub: h, identities: map[int64]string{42: "claw"}}
	if err := (&uiTransport{d: d}).SendMessage("tg:42", "status", "", ""); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	ev, _, _ := h.collect("claw", h.epoch, 0)
	if len(ev) != 1 {
		t.Fatalf("canonical user got %d events, want 1 notice", len(ev))
	}
	if e := decodeEvent(t, ev[0]); e.Type != "notice" || e.Text != "status" {
		t.Fatalf("event = %+v, want status notice", e)
	}
	if raw, _, _ := h.collect("42", h.epoch, 0); len(raw) != 0 {
		t.Fatal("raw messenger id received the canonical UI notice")
	}
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

	// A fresh long-poll (no cursor) returns immediately with a cursor to start from.
	poll := httptest.NewRecorder()
	preq := httptest.NewRequest("GET", "/api/poll", nil)
	preq.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(poll, preq)
	if poll.Code != 200 || !strings.Contains(poll.Body.String(), `"cursor"`) {
		t.Fatalf("fresh /api/poll: code=%d body=%s", poll.Code, poll.Body.String())
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
