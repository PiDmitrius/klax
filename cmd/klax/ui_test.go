package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/runner"
)

func decodeEvent(t *testing.T, raw json.RawMessage) uiEventJSON {
	t.Helper()
	var ev uiEventJSON
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

// Ring events get one monotonic seq; collect returns everything after a cursor for the
// request's role, never another user's events.
func TestUIRingCollect(t *testing.T) {
	h := newUIHub()
	for i := 0; i < 5; i++ {
		h.notice("alice", "x")
	}
	ev, at, resync := h.collect("alice", 0, roleRW)
	if resync || at != 5 || len(ev) != 5 || decodeEvent(t, ev[0]).Seq != 1 {
		t.Fatalf("collect(0): n=%d at=%d resync=%v", len(ev), at, resync)
	}
	if ev, _, _ := h.collect("alice", 2, roleRO); len(ev) != 3 || decodeEvent(t, ev[0]).Seq != 3 {
		t.Fatalf("collect(2) = %d events", len(ev))
	}
	if ev, _, resync := h.collect("alice", 5, roleRW); resync || len(ev) != 0 {
		t.Fatalf("collect(5): n=%d resync=%v", len(ev), resync)
	}
	if ev, _, _ := h.collect("bob", 0, roleRW); len(ev) != 0 {
		t.Fatalf("bob got %d of alice's events", len(ev))
	}
}

// The ring evicts while it is over the soft byte bound and keeps the hard minimum of events.
// Evicting one role's event raises only that role's floor, so the other role never resyncs for it.
func TestUIRingEvictionFloorsPerRole(t *testing.T) {
	h := newUIHub()
	u := h.userSync("alice")
	big := json.RawMessage(`"` + strings.Repeat("x", uiRingSoftBytes/uiRingMinEvents) + `"`)
	h.mu.Lock()
	for i := 0; i < 2*uiRingMinEvents; i++ {
		h.appendLocked("alice", u, []uiPending{{role: roleRO, ev: uiEventJSON{Sessions: big}}})
	}
	h.mu.Unlock()
	if len(u.ring) < uiRingMinEvents || u.floor[roleRO] == 0 {
		t.Fatalf("ring=%d floorRO=%d: want eviction down to at least the minimum", len(u.ring), u.floor[roleRO])
	}
	if u.floor[roleRW] != 0 {
		t.Fatalf("read-only events raised the full-access floor to %d", u.floor[roleRW])
	}
	if _, _, resync := h.collect("alice", 0, roleRW); resync {
		t.Fatal("full-access client resynced for evicted read-only events")
	}
	if _, _, resync := h.collect("alice", 0, roleRO); !resync {
		t.Fatal("read-only client behind its floor must resync")
	}
	h.mu.Lock()
	for i := 0; i < 2*uiRingMinEvents; i++ {
		h.appendLocked("alice", u, []uiPending{{role: roleShared, ev: uiEventJSON{Sessions: big}}})
	}
	h.mu.Unlock()
	if u.floor[roleRW] == 0 {
		t.Fatal("evicted shared events must raise every role's floor")
	}
	if len(u.ring) < uiRingMinEvents {
		t.Fatalf("ring kept %d events, below the hard minimum", len(u.ring))
	}
}

// Appending an event wakes the user's held polls.
func TestUIHubWakeOnNotice(t *testing.T) {
	h := newUIHub()
	ch := h.waitChan("alice")
	h.notice("alice", "x")
	select {
	case <-ch:
	default:
		t.Fatal("a notice did not wake a held poll")
	}
}

// Before a restart the drain waits only for polls whose cursor is still before the notice.
func TestWaitPollsPastReleasesWhenPollsMoveOn(t *testing.T) {
	h := newUIHub()
	h.userSync("alice")
	behind := h.registerPoll(0)
	ahead := h.registerPoll(100)
	seq := h.noticeAll("restart")
	done := make(chan struct{})
	go func() { h.waitPollsPast(seq, time.Second); close(done) }()
	select {
	case <-done:
		t.Fatal("wait returned while a poll was still behind the notice")
	case <-time.After(10 * time.Millisecond):
	}
	h.unregisterPoll(behind)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wait did not release once no poll was behind the notice")
	}
	h.unregisterPoll(ahead)
}

func TestUICursorRoundTrip(t *testing.T) {
	h := newUIHub()
	if seq, ok := h.parseAfter(h.cursor(42)); !ok || seq != 42 {
		t.Fatalf("parseAfter(cursor(42)) = %d, %v", seq, ok)
	}
	for _, v := range []string{"", "42", fmt.Sprintf("%d.42", h.epoch+1), fmt.Sprintf("%d.x", h.epoch)} {
		if _, ok := h.parseAfter(v); ok {
			t.Fatalf("parseAfter(%q) accepted a cursor from another process or a malformed one", v)
		}
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
	if len(tokens) != 2 || tokens["secret1"].User != "alice" || tokens["secret2"].User != "bob" {
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
	s := &uiServer{tokens: map[string]uiAccess{"secret": {User: "alice"}}}

	bearer := httptest.NewRequest("GET", "/api/state", nil)
	bearer.Header.Set("Authorization", "Bearer secret")
	if u, ok := s.auth(bearer); !ok || u != "alice" {
		t.Fatalf("bearer auth: got %q ok=%v", u, ok)
	}

	// A query-string token is NOT accepted: every UI request (incl. the changes long-poll)
	// sets the Authorization header, so there is no ?token= auth path.
	query := httptest.NewRequest("GET", "/api/state?token=secret", nil)
	if _, ok := s.auth(query); ok {
		t.Fatal("query token must not authenticate")
	}

	bad := httptest.NewRequest("GET", "/api/state", nil)
	bad.Header.Set("Authorization", "Bearer nope")
	if _, ok := s.auth(bad); ok {
		t.Fatal("unknown token must not authenticate")
	}

	none := httptest.NewRequest("GET", "/api/state", nil)
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

// newUIDelivery pokes the CANONICAL user's held polls (user:alice -> "alice"), never the raw messenger id.
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

	ev, _, _ := h.collect("alice", 0, roleRW)
	if len(ev) != 1 {
		t.Fatalf("canonical user got %d events, want 1 notice", len(ev))
	}
	if e := decodeEvent(t, ev[0]); e.Notice != "status" {
		t.Fatalf("event = %+v, want status notice", e)
	}
	if raw, _, _ := h.collect("42", 0, roleRW); len(raw) != 0 {
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
	h := (&uiServer{d: d, tokens: map[string]uiAccess{"sec": {User: "alice"}}}).routes()

	spa := httptest.NewRecorder()
	h.ServeHTTP(spa, httptest.NewRequest("GET", "/", nil))
	if spa.Code != 200 || !strings.Contains(spa.Body.String(), "klax") {
		t.Fatalf("SPA: code=%d", spa.Code)
	}

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest("GET", "/api/state", nil))
	if unauth.Code != 401 {
		t.Fatalf("unauthenticated /api/state: code=%d, want 401", unauth.Code)
	}

	ok := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(ok, req)
	if ok.Code != 200 || !strings.Contains(ok.Body.String(), `"created"`) {
		t.Fatalf("authenticated /api/state: code=%d body=%s", ok.Code, ok.Body.String())
	}

	// The changes long-poll is wired and authenticated: a malformed body reaches handleChanges and
	// is rejected 400 (not 404/401), proving the route past auth without holding the poll open.
	changes := httptest.NewRecorder()
	creq0 := httptest.NewRequest("POST", "/api/changes", strings.NewReader("not json"))
	creq0.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(changes, creq0)
	if changes.Code != 400 {
		t.Fatalf("/api/changes with a bad body: code=%d, want 400", changes.Code)
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
	h := (&uiServer{d: d, tokens: map[string]uiAccess{"sec": {User: "alice"}}}).routes()
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
	h := (&uiServer{d: d, tokens: map[string]uiAccess{"sec": {User: "alice"}}}).routes()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/abort", strings.NewReader(`{"session":99999}`))
	req.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("/api/abort on a missing session: code=%d, want 404", rec.Code)
	}
}

func TestUICancelRejectsNotQueued(t *testing.T) {
	d := &daemon{store: newStoreWithChat("user:alice", "one"), runners: make(map[runnerKey]*sessionRunner)}
	created := d.store.SessionsFor("user:alice")[0].Created
	h := (&uiServer{d: d, tokens: map[string]uiAccess{"sec": {User: "alice"}}}).routes()
	for body, want := range map[string]int{
		`{"session":99999,"seq":1}`:                    404,
		fmt.Sprintf(`{"session":%d,"seq":1}`, created): 409,
		fmt.Sprintf(`{"session":%d}`, created):         400,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/cancel", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sec")
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("/api/cancel %s: code=%d, want %d", body, rec.Code, want)
		}
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

func TestHandleSPAManifestUsesConfiguredTitle(t *testing.T) {
	s := &uiServer{d: &daemon{cfg: &config.Config{UITitle: `KLODIN "mobile"`}}}
	rec := httptest.NewRecorder()
	s.handleSPA(rec, httptest.NewRequest("GET", "/manifest.webmanifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/manifest+json" {
		t.Fatalf("manifest content-type=%q", ct)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Fatalf("manifest cache-control=%q", got)
	}
	var manifest struct {
		Name       string `json:"name"`
		ShortName  string `json:"short_name"`
		StartURL   string `json:"start_url"`
		Scope      string `json:"scope"`
		Display    string `json:"display"`
		ThemeColor string `json:"theme_color"`
		Background string `json:"background_color"`
		Icons      []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Name != `KLODIN "mobile"` || manifest.ShortName != manifest.Name {
		t.Fatalf("manifest names = %q / %q", manifest.Name, manifest.ShortName)
	}
	if manifest.StartURL != "." || manifest.Scope != "." || manifest.Display != "standalone" {
		t.Fatalf("manifest routing/display = %+v", manifest)
	}
	if manifest.ThemeColor != "#f5f6f8" || manifest.Background != "#f5f6f8" || len(manifest.Icons) != 4 {
		t.Fatalf("manifest theme/icons = %+v", manifest)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw manifest: %v", err)
	}
	if _, ok := raw["id"]; ok {
		t.Fatal("manifest must omit id: a relative id resolves at the origin and loses a reverse-proxy prefix")
	}
	maskable := 0
	for _, icon := range manifest.Icons {
		if icon.Type != "image/png" {
			t.Fatalf("icon %s type = %q", icon.Src, icon.Type)
		}
		if icon.Purpose == "maskable" {
			maskable++
		}
		path := "/" + strings.TrimPrefix(icon.Src, "./")
		iconRec := httptest.NewRecorder()
		s.handleSPA(iconRec, httptest.NewRequest("GET", path, nil))
		if iconRec.Code != http.StatusOK || iconRec.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("icon %s: code %d, content-type %q", icon.Src, iconRec.Code, iconRec.Header().Get("Content-Type"))
		}
		if got := iconRec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Fatalf("icon %s cache-control=%q", icon.Src, got)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(iconRec.Body.Bytes()))
		if err != nil {
			t.Fatalf("decode icon %s: %v", icon.Src, err)
		}
		actual := fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
		if actual != icon.Sizes {
			t.Fatalf("icon %s declares %s, actual %s", icon.Src, icon.Sizes, actual)
		}
	}
	if maskable != 1 {
		t.Fatalf("manifest has %d maskable icons, want 1", maskable)
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
