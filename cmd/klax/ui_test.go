package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/runner"
)

func decodeEvent(t *testing.T, raw json.RawMessage) uiEvent {
	t.Helper()
	var ev uiEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	return ev
}

func TestQueuedCountExcludesFirstIdleQueuedMessage(t *testing.T) {
	d := &daemon{runners: map[runnerKey]*sessionRunner{}}
	sr := &sessionRunner{runner: runner.New(), queue: []queuedMsg{{turnSeq: 1}}}
	d.runners[runnerKey{sk: "user:x", created: 1}] = sr
	if got := d.queuedCount("user:x", 1); got != 0 {
		t.Fatalf("idle first queued count = %d, want 0", got)
	}
	sr.processing = true
	if got := d.queuedCount("user:x", 1); got != 1 {
		t.Fatalf("processing queued count = %d, want 1", got)
	}
}

// Every retained event (a notice — the only kind kept now) gets a monotonic seq per user; collect
// returns everything after a cursor, the head, and whether the cursor is coverable.
func TestUIHubCollect(t *testing.T) {
	h := newUIHub()
	for i := 0; i < 5; i++ {
		h.broadcast("alice", uiEvent{Type: "notice", Text: "x"})
	}
	// From cursor 0: all 5, head 5, no reload.
	ev, head, reload := h.collect("alice", h.epoch, 0)
	if reload || head != 5 || len(ev) != 5 {
		t.Fatalf("collect(0): n=%d head=%d reload=%v, want 5/5/false", len(ev), head, reload)
	}
	if first := decodeEvent(t, ev[0]); first.Seq != 1 {
		t.Fatalf("first seq=%d, want 1", first.Seq)
	}
	// From cursor 2: exactly 3,4,5.
	if ev, _, _ := h.collect("alice", h.epoch, 2); len(ev) != 3 || decodeEvent(t, ev[0]).Seq != 3 {
		t.Fatalf("collect(2): n=%d first=%d, want 3 starting at seq 3", len(ev), decodeEvent(t, ev[0]).Seq)
	}
	// Up to date: nothing, no reload.
	if ev, _, reload := h.collect("alice", h.epoch, 5); reload || len(ev) != 0 {
		t.Fatalf("collect(5): n=%d reload=%v, want 0/false", len(ev), reload)
	}
	// Stale epoch (daemon restart) -> reload.
	if _, _, reload := h.collect("alice", h.epoch+1, 2); !reload {
		t.Fatal("a stale-epoch cursor must report reload")
	}
}

// A notice cursor older than the retained ring (overflow) -> reload (transcript backstop).
func TestUIHubCollectGapOnOverflow(t *testing.T) {
	h := newUIHub()
	for i := 0; i < uiRingMaxItems+50; i++ {
		h.broadcast("alice", uiEvent{Type: "notice", Text: "x"})
	}
	if _, _, reload := h.collect("alice", h.epoch, 1); !reload {
		t.Fatal("a cursor predating the ring must report reload")
	}
}

// (The user-echo-via-ring and delivery-emits-error tests were removed with the ring content
// channel: a turn's user bubble and its aborted/error block now come from the durable log via
// buildReadModel, delivered by the tail — covered by the tailFrom/unreadAfter tests.)

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
	ch := h.waitChan("alice")
	select {
	case <-ch:
		t.Fatal("wake channel fired before any emit")
	default:
	}
	h.broadcast("alice", uiEvent{Type: "progress", Text: "x"})
	select {
	case <-ch: // emit closed the channel we were holding
	default:
		t.Fatal("emit did not wake a held waiter")
	}
}

// bumpSessions advances the per-user strip revision AND wakes held polls, so handleTail can return
// a sessions-only change (rename/create/close/cross-tab read) immediately instead of holding to
// the timeout.
func TestBumpSessionsAdvancesRevAndWakes(t *testing.T) {
	h := newUIHub()
	if h.sessionsRev("alice") != 0 {
		t.Fatalf("fresh rev = %d, want 0", h.sessionsRev("alice"))
	}
	ch := h.waitChan("alice")
	h.bumpSessions("alice")
	if got := h.sessionsRev("alice"); got != 1 {
		t.Fatalf("rev after bump = %d, want 1", got)
	}
	select {
	case <-ch: // the bump woke the held poll
	default:
		t.Fatal("bumpSessions did not wake a held poll")
	}
}

// A daemon-wide notice must reach a user who polled at least once but is between polls now (no
// inflight, and no ring entry since content isn't retained) — else the restart/update banner is
// silently dropped for an open UI.
func TestBroadcastAllReachesKnownUserBetweenPolls(t *testing.T) {
	h := newUIHub()
	h.enterPoll("alice") // marks the user known
	h.leavePoll("alice") // now: not inflight, no ring entry
	h.broadcastAll(uiEvent{Type: "notice", Text: "restart"})
	if ev, _, _ := h.collect("alice", h.epoch, 0); len(ev) != 1 {
		t.Fatalf("known user between polls missed the notice: got %d events, want 1", len(ev))
	}
}

// Concurrently-held polls per user are capped (bounds goroutines without
// per-connection state); a freed slot allows a new poll.
func TestUIHubInflightCap(t *testing.T) {
	h := newUIHub()
	for i := 0; i < uiMaxInflightPerUser; i++ {
		if !h.enterPoll("alice") {
			t.Fatalf("enterPoll refused at %d, under the cap", i)
		}
	}
	if h.enterPoll("alice") {
		t.Fatal("enterPoll must refuse past the cap")
	}
	h.leavePoll("alice")
	if !h.enterPoll("alice") {
		t.Fatal("a freed slot must allow a new poll")
	}
}

func TestBuildUITokens(t *testing.T) {
	tokens, err := buildUITokens([]config.UserIdentity{
		{ID: "alice", UIToken: "secret1"},
		{ID: "bob", UIToken: "secret2"},
		{ID: "noui"}, // no token — skipped, not an error
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 2 || tokens["secret1"] != "alice" || tokens["secret2"] != "bob" {
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
	s := &uiServer{tokens: map[string]string{"secret": "alice"}}

	bearer := httptest.NewRequest("GET", "/api/sessions", nil)
	bearer.Header.Set("Authorization", "Bearer secret")
	if u, ok := s.auth(bearer); !ok || u != "alice" {
		t.Fatalf("bearer auth: got %q ok=%v", u, ok)
	}

	// A query-string token is NOT accepted: every UI request (incl. the tail long-poll)
	// sets the Authorization header, so there is no ?token= auth path.
	query := httptest.NewRequest("GET", "/api/tail?token=secret", nil)
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
	d := &daemon{identities: map[int64]string{42: "alice"}}
	if got := d.sessionKey("ui:alice"); got != "user:alice" {
		t.Fatalf("ui sessionKey = %q, want user:alice", got)
	}
	if got := d.sessionKey("tg:42"); got != "user:alice" {
		t.Fatalf("tg sessionKey = %q, want user:alice (shared identity)", got)
	}
}

func TestDeliveryForRoutesUIChat(t *testing.T) {
	d := &daemon{uiHub: newUIHub()}
	del := d.deliveryFor(context.Background(), queuedMsg{chatID: "ui:alice", sessCreated: 5}, true)
	if _, ok := del.(*uiDelivery); !ok {
		t.Fatalf("ui chat must get *uiDelivery, got %T", del)
	}
	del.Close()
}

// A messenger turn on a canonical "user:" session is mirrored to the web UI (tee): its delivery
// POKES the canonical user's tail so the answer/progress stream there live, not only on reload. A
// raw (unmapped) messenger session is not mirrored, and never pokes a UI hub.
func TestDeliveryForMirrorsMessengerToUI(t *testing.T) {
	d := newTestDeliveryDaemon(&fakeTransport{})
	d.uiHub = newUIHub()

	canon := d.uiHub.waitChan("alice")
	del := d.deliveryFor(context.Background(), queuedMsg{chatID: "tg:1", sessKey: "user:alice", sessCreated: 7}, true)
	if _, ok := del.(teeDelivery); !ok {
		t.Fatalf("canonical messenger session must mirror to UI (teeDelivery), got %T", del)
	}
	del.Close()
	select {
	case <-canon: // the tee's newUIDelivery poked the canonical UI hub
	default:
		t.Fatal("messenger turn not mirrored (no poke) to the canonical UI hub")
	}

	raw := d.uiHub.waitChan("2")
	del2 := d.deliveryFor(context.Background(), queuedMsg{chatID: "tg:2", sessKey: "tg:2", sessCreated: 9}, true)
	if _, ok := del2.(teeDelivery); ok {
		t.Fatal("a raw (unmapped) messenger session must NOT mirror to UI")
	}
	del2.Close()
	select {
	case <-raw:
		t.Fatal("raw messenger session leaked a poke to a UI hub")
	default:
	}
}

// newUIDelivery pokes the CANONICAL user's tail (user:alice -> "alice"), never the raw messenger id.
func TestUIDeliveryUsesCanonicalSessionUser(t *testing.T) {
	h := newUIHub()
	d := &daemon{uiHub: h}
	canon := h.waitChan("alice")
	raw := h.waitChan("42")
	d.newUIDelivery(context.Background(), queuedMsg{chatID: "tg:42", sessKey: "user:alice", sessCreated: 7})

	select {
	case <-canon: // poked the canonical user
	default:
		t.Fatal("newUIDelivery did not poke the canonical user")
	}
	select {
	case <-raw:
		t.Fatal("newUIDelivery poked the raw messenger id instead of the canonical user")
	default:
	}
}

func TestUITransportUsesCanonicalSessionUser(t *testing.T) {
	h := newUIHub()
	d := &daemon{uiHub: h, identities: map[int64]string{42: "alice"}}
	if err := (&uiTransport{d: d}).SendMessage("tg:42", "status", "", ""); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	ev, _, _ := h.collect("alice", h.epoch, 0)
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
		store:   newStoreWithChat("user:alice", "one"),
		uiHub:   newUIHub(),
		runners: make(map[runnerKey]*sessionRunner),
	}
	h := (&uiServer{d: d, tokens: map[string]string{"sec": "alice"}}).routes()

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

	// The tail long-poll is wired and authenticated: a malformed body reaches handleTail and is
	// rejected 400 (not 404/401), proving the route past auth without holding the poll open.
	tail := httptest.NewRecorder()
	treq := httptest.NewRequest("POST", "/api/tail", strings.NewReader("not json"))
	treq.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(tail, treq)
	if tail.Code != 400 {
		t.Fatalf("/api/tail with a bad body: code=%d, want 400 (route reached handleTail)", tail.Code)
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
	d := &daemon{cfg: &config.Config{}, store: newStoreWithChat("user:alice", "one"),
		uiHub: newUIHub(), runners: make(map[runnerKey]*sessionRunner)}
	h := (&uiServer{d: d, tokens: map[string]string{"sec": "alice"}}).routes()
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
	d := &daemon{store: newStoreWithChat("user:alice", "one"), runners: make(map[runnerKey]*sessionRunner)}
	h := (&uiServer{d: d, tokens: map[string]string{"sec": "alice"}}).routes()
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
