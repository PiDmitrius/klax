package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/history"
	"github.com/PiDmitrius/klax/internal/session"
)

// uiPrefix is the chatID/transport prefix for the web UI. A request authenticated
// as canonical user "claw" is handled as chatID "ui:claw", and sessionKey maps
// that to "user:claw" — the same key tg/mx DMs for claw resolve to, so the UI
// shares sessions with the messengers (cross-channel continuity).
const uiPrefix = "ui"

const uiClientBuffer = 64

// uiSessionInfo is one tab in the strip.
type uiSessionInfo struct {
	Created   int64  `json:"created"`
	Name      string `json:"name"`
	Active    bool   `json:"active"`
	Busy      bool   `json:"busy"`
	Queued    int    `json:"queued"` // messages waiting behind the running one
	Backend   string `json:"backend"`
	Model     string `json:"model"`
	CWD       string `json:"cwd"`
	Messages  int    `json:"messages"`
	CtxUsed   int    `json:"ctx_used"`
	CtxWindow int    `json:"ctx_window"`
}

// uiEvent is one server-sent event. The client multiplexes all tabs over a
// single stream and routes by Session (a session's Created).
type uiEvent struct {
	Type      string          `json:"type"` // sessions|turn_start|progress|final|error|notice|compact
	Session   int64           `json:"session,omitempty"`
	Kind      string          `json:"kind,omitempty"` // progress: tool|narration
	Text      string          `json:"text,omitempty"`
	Markdown  string          `json:"markdown,omitempty"`
	Sessions  []uiSessionInfo `json:"sessions,omitempty"`
	Model     string          `json:"model,omitempty"`
	CtxUsed   int             `json:"ctx_used,omitempty"`
	CtxWindow int             `json:"ctx_window,omitempty"`
}

// userFromKey pulls the canonical user out of a session key ("user:claw") or a
// UI chatID/raw ("ui:claw" or "claw"). Messenger keys like "tg:123" return
// "123", which never matches a UI client, so they are harmless no-ops.
func userFromKey(s string) string {
	if i := strings.Index(s, ":"); i != -1 {
		return s[i+1:]
	}
	return s
}

// uiHub fans server events out to connected SSE clients, keyed by canonical user.
type uiHub struct {
	mu      sync.Mutex
	clients map[*uiClient]struct{}
}

type uiClient struct {
	user string
	ch   chan []byte
}

func newUIHub() *uiHub { return &uiHub{clients: make(map[*uiClient]struct{})} }

func (h *uiHub) subscribe(user string) *uiClient {
	c := &uiClient{user: user, ch: make(chan []byte, uiClientBuffer)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *uiHub) unsubscribe(c *uiClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// broadcast delivers payload to every client of user. A client whose buffer is
// full is dropped (its channel closed, its SSE writer then exits) rather than
// blocking the caller — progress events come from the runner's stdout goroutine,
// which must never block. The dropped client reconnects and re-syncs via
// /api/sessions + /api/transcript, so liveness is best-effort by design.
func (h *uiHub) broadcast(user string, payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.user != user {
			continue
		}
		select {
		case c.ch <- payload:
		default:
			close(c.ch)
			delete(h.clients, c)
		}
	}
}

// broadcastAll delivers payload to every connected client (any user). Used for
// daemon-wide banners (restart/update). Same slow-client drop as broadcast.
func (h *uiHub) broadcastAll(payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.ch <- payload:
		default:
			close(c.ch)
			delete(h.clients, c)
		}
	}
}

// uiNotifyAll pushes a notice to every connected UI client (any user).
func (d *daemon) uiNotifyAll(text string) {
	if d.uiHub == nil {
		return
	}
	payload, err := json.Marshal(uiEvent{Type: "notice", Text: text})
	if err != nil {
		return
	}
	d.uiHub.broadcastAll(payload)
}

// recentStartupNotice returns the post-restart banner for a UI client connecting
// shortly after startup — the original broadcast had no UI clients yet — or ""
// once the short window has passed (so a later fresh open sees nothing stale).
func (d *daemon) recentStartupNotice() string {
	if d.startupNotice == "" || time.Since(d.startedAt) > 90*time.Second {
		return ""
	}
	return d.startupNotice
}

// uiEmit marshals and broadcasts one event to a user. No-op when the UI is off.
func (d *daemon) uiEmit(user string, ev uiEvent) {
	if d.uiHub == nil || user == "" {
		return
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		log.Printf("ui: marshal event: %v", err)
		return
	}
	d.uiHub.broadcast(user, payload)
}

// broadcastSessions pushes the current tab-strip snapshot to the UI client(s)
// watching this session key. Cheap no-op when the UI is off or nobody watches.
func (d *daemon) broadcastSessions(sk string) {
	if d.uiHub == nil {
		return
	}
	d.uiEmit(userFromKey(sk), uiEvent{Type: "sessions", Sessions: d.sessionsSnapshot(sk)})
}

func (d *daemon) uiUserForChat(chatID string) string {
	return userFromKey(d.sessionKey(chatID))
}

// queuedCount is the number of messages waiting in a session's queue (excludes
// the one currently running).
func (d *daemon) queuedCount(sk string, created int64) int {
	sr := d.lookupRunner(sk, created)
	if sr == nil {
		return 0
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return len(sr.queue)
}

// newUISession creates a fresh active session for a UI chat and returns it so
// the caller can switch the client to it.
func (d *daemon) newUISession(sk, chatID string) *session.Session {
	sess, _ := d.createSession(chatID, sk, "session")
	d.saveStore()
	d.broadcastSessions(sk)
	return sess
}

// renameSession renames one session (by Created) and pushes the updated tab strip.
func (d *daemon) renameSession(sk string, created int64, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if d.store.UpdateSession(sk, created, func(cur *session.Session) { cur.Name = name }) == nil {
		return false
	}
	d.saveStore()
	d.broadcastSessions(sk)
	return true
}

// closeSession aborts any in-flight run on a session and removes it (the
// transcript JSONL stays on disk). Refuses the last remaining session; promotes
// a new active one if the closed tab was active. Mirrors the /nuke teardown
// order (abort → delete → dropRunner).
func (d *daemon) closeSession(sk string, created int64) error {
	sessions := d.store.SessionsFor(sk)
	if len(sessions) <= 1 {
		return errors.New("нельзя закрыть последнюю сессию")
	}
	idx, wasActive := -1, false
	for i, s := range sessions {
		if s.Created == created {
			idx, wasActive = i, s.Active
			break
		}
	}
	if idx == -1 {
		return errors.New("сессия не найдена")
	}
	d.abortSession(sk, created, true)
	d.store.Delete(sk, idx)
	d.dropRunner(sk, created)
	if wasActive {
		d.store.Switch(sk, 0) // promote the first remaining session
	}
	d.saveStore()
	d.broadcastSessions(sk)
	return nil
}

func (d *daemon) sessionsSnapshot(sk string) []uiSessionInfo {
	sessions := d.store.SessionsFor(sk)
	out := make([]uiSessionInfo, 0, len(sessions))
	for _, s := range sessions {
		backend := s.Backend
		if backend == "" {
			backend = "claude"
		}
		model := s.ModelOverride
		if model == "" {
			model = s.Model
		}
		out = append(out, uiSessionInfo{
			Created:   s.Created,
			Name:      s.Name,
			Active:    s.Active,
			Busy:      d.isSessionBusy(sk, s.Created),
			Queued:    d.queuedCount(sk, s.Created),
			Backend:   backend,
			Model:     model,
			CWD:       s.CWD,
			Messages:  s.Messages,
			CtxUsed:   s.ContextUsed,
			CtxWindow: s.ContextWindow,
		})
	}
	return out
}

// buildUITokens maps each configured ui_token to its canonical user. It rejects
// an empty id on a token-bearing user and duplicate tokens; the token itself is
// never echoed into the error (privacy).
func buildUITokens(users []config.UserIdentity) (map[string]string, error) {
	tokens := make(map[string]string)
	for _, u := range users {
		if u.UIToken == "" {
			continue
		}
		if u.ID == "" {
			return nil, fmt.Errorf("ui: user with a ui_token has an empty id")
		}
		if _, dup := tokens[u.UIToken]; dup {
			return nil, fmt.Errorf("ui: duplicate ui_token in config")
		}
		tokens[u.UIToken] = u.ID
	}
	return tokens, nil
}

// uiTransport adapts the web UI to transport.Transport so every existing reply
// path (sendMessage/sendPlain, command output, errors) reaches the UI as a
// "notice" event without touching those call sites. It is registered in
// d.transports["ui"] but deliberately excluded from /transports (it is not a
// pollable messenger).
type uiTransport struct {
	d *daemon
}

func (t *uiTransport) SendMessage(chatID, text, replyTo, format string) error {
	t.d.uiEmit(t.d.uiUserForChat(chatID), uiEvent{Type: "notice", Text: text})
	return nil
}

func (t *uiTransport) SendMessageReturnID(chatID, text, replyTo, format string) (string, error) {
	t.d.uiEmit(t.d.uiUserForChat(chatID), uiEvent{Type: "notice", Text: text})
	return "ui-notice", nil
}

func (t *uiTransport) EditMessage(chatID, messageID, text, format string) error {
	t.d.uiEmit(t.d.uiUserForChat(chatID), uiEvent{Type: "notice", Text: text})
	return nil
}

// uiServer is the HTTP/SSE Source. It binds 127.0.0.1 (per config), serves the
// SPA and the JSON API, and authenticates every request by bearer token.
type uiServer struct {
	d      *daemon
	addr   string
	tokens map[string]string // token -> canonical user
}

func (s *uiServer) Name() string { return uiPrefix }

func (s *uiServer) Run(ctx context.Context) {
	srv := &http.Server{Addr: s.addr, Handler: s.routes()}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	if !isLoopbackAddr(s.addr) {
		log.Printf("ui: WARNING %q is not loopback — the bearer token travels in cleartext (it is also in the SSE URL); keep ui_listen on 127.0.0.1 or front it with a TLS proxy", s.addr)
	}
	log.Printf("ui: listening on %s", s.addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("ui: server error: %v", err)
	}
}

// isLoopbackAddr reports whether a listen address binds only the loopback
// interface. An empty host (e.g. ":8799") binds all interfaces and is not.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (s *uiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/abort", s.handleAbort)
	mux.HandleFunc("/api/new", s.handleNew)
	mux.HandleFunc("/api/rename", s.handleRename)
	mux.HandleFunc("/api/close", s.handleClose)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/transcript", s.handleTranscript)
	mux.HandleFunc("/emoji/", s.handleEmoji)
	mux.HandleFunc("/", s.handleSPA)
	return mux
}

// auth resolves the bearer token (Authorization header) to a canonical user.
// Query-string tokens are deliberately NOT accepted here: only the SSE stream
// needs one (EventSource cannot set headers — see authSSE). Accepting ?token= on
// POST/data routes would needlessly widen the token-in-URL leakage surface.
func (s *uiServer) auth(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	u, ok := s.tokens[strings.TrimPrefix(h, "Bearer ")]
	return u, ok
}

// authSSE additionally accepts ?token= because EventSource cannot set an
// Authorization header. Used ONLY by the SSE endpoint.
func (s *uiServer) authSSE(r *http.Request) (string, bool) {
	if u, ok := s.auth(r); ok {
		return u, true
	}
	if tok := r.URL.Query().Get("token"); tok != "" {
		if u, ok := s.tokens[tok]; ok {
			return u, true
		}
	}
	return "", false
}

func (s *uiServer) chatID(user string) string { return uiPrefix + ":" + user }

func (s *uiServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authSSE(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	c := s.d.uiHub.subscribe(user)
	defer s.d.uiHub.unsubscribe(c)

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// Initial tab strip (ensures at least one session exists first).
	sk := s.d.sessionKey(s.chatID(user))
	s.d.ensureSessionWithCWD(sk, s.d.sessionCWD(s.chatID(user)))
	s.d.broadcastSessions(sk)

	// If the daemon just restarted/updated, hand this (re)connecting client the
	// startup banner — it was broadcast before any UI client could reconnect.
	if note := s.d.recentStartupNotice(); note != "" {
		if payload, err := json.Marshal(uiEvent{Type: "notice", Text: note}); err == nil {
			select {
			case c.ch <- payload:
			default:
			}
		}
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case payload, ok := <-c.ch:
			if !ok {
				return // dropped as a slow client
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		case <-heartbeat.C:
			// A real data event, not an SSE ": ping" comment: EventSource.onmessage
			// never fires for comment lines, so the client's liveness watchdog can
			// only observe a data frame. Lets it detect a dead-but-held-open stream
			// (e.g. behind a reverse proxy after an abrupt daemon exit) and reconnect.
			fmt.Fprint(w, "data: {\"type\":\"ping\"}\n\n")
			flusher.Flush()
		}
	}
}

func (s *uiServer) handleSend(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// While draining for a restart the daemon refuses new turns (enqueue drops them
	// with only an SSE notice). Tell the client explicitly so it rolls back the
	// optimistic echo instead of leaving a ghost message that never reaches a turn
	// and lingers in localStorage until the reconcile TTL.
	if s.d.isDraining() {
		http.Error(w, "klax перезапускается — попробуйте через минуту", http.StatusServiceUnavailable)
		return
	}
	// Cap the whole request body (attachments included) so a buggy or hostile
	// client cannot exhaust memory/disk while parsing.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	var (
		text          string
		targetCreated int64
		attachments   []attachment
	)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		text = r.FormValue("text")
		targetCreated, _ = strconv.ParseInt(r.FormValue("session"), 10, 64)
		for _, fh := range r.MultipartForm.File["files"] {
			f, err := fh.Open()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				continue
			}
			attachments = append(attachments, attachment{filename: fh.Filename, data: data})
		}
	} else {
		var body struct {
			Session int64  `json:"session"`
			Text    string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		text = body.Text
		targetCreated = body.Session
	}
	// The UI always targets a specific tab; never silently fall back to the
	// active session the way the messenger paths do.
	if targetCreated <= 0 {
		http.Error(w, "a positive session is required", http.StatusBadRequest)
		return
	}
	// Refuse a send to a tab whose session no longer exists (e.g. closed from
	// another client): enqueue would drop it with only an SSE notice while we'd
	// still answer 204, stranding the client's optimistic echo as a ghost. A 404
	// lets the client roll it back. (Mirrors handleAbort's existence check.)
	if s.d.store.Get(s.d.sessionKey(s.chatID(user)), targetCreated) == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.d.handleInbound(Inbound{
		ChatID:        s.chatID(user),
		Text:          text,
		Attachments:   attachments,
		TargetCreated: targetCreated,
		RawMessage:    true, // the UI has no chat commands — "/"-text is a message
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *uiServer) handleAbort(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Session int64 `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Session <= 0 {
		http.Error(w, "a positive session is required", http.StatusBadRequest)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	if s.d.store.Get(sk, body.Session) == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.d.abortSession(sk, body.Session, false)
	w.WriteHeader(http.StatusNoContent)
}

func (s *uiServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	s.d.ensureSessionWithCWD(sk, s.d.sessionCWD(s.chatID(user)))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.d.sessionsSnapshot(sk))
}

func (s *uiServer) handleNew(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	sess := s.d.newUISession(sk, s.chatID(user))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Created int64 `json:"created"`
	}{sess.Created})
}

func (s *uiServer) handleRename(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Session int64  `json:"session"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Session <= 0 || strings.TrimSpace(body.Name) == "" {
		http.Error(w, "session and name are required", http.StatusBadRequest)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	if !s.d.renameSession(sk, body.Session, body.Name) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *uiServer) handleClose(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Session int64 `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Session <= 0 {
		http.Error(w, "a positive session is required", http.StatusBadRequest)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	if err := s.d.closeSession(sk, body.Session); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTranscript returns a tab's history, paginated from the end: the newest
// `limit` turns, with older ones fetched lazily by passing the returned offset
// back as `before`. Reading the whole JSONL bounds the response, not the read,
// which is fine for a single local user; true reverse-streaming is a later
// optimization.
func (s *uiServer) handleTranscript(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	created, _ := strconv.ParseInt(r.URL.Query().Get("session"), 10, 64)
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	sk := s.d.sessionKey(s.chatID(user))
	sess := s.d.store.Get(sk, created)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	backend := sess.Backend
	if backend == "" {
		backend = "claude"
	}
	items, err := history.Load(backend, sess.ID, sess.CWD)
	if err != nil {
		log.Printf("ui: transcript load: %v", err)
		items = nil
	}
	end := len(items)
	if before > 0 && int(before) < end {
		end = int(before)
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	turns := items[start:end]
	if turns == nil {
		turns = []history.Item{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Turns  []history.Item `json:"turns"`
		More   bool           `json:"more"`
		Offset int            `json:"offset"`
	}{Turns: turns, More: start > 0, Offset: start})
}

// handleEmoji serves a bundled color-emoji web-font subset (woff2). No auth —
// it is a static asset like the SPA shell; the filename is constrained to a
// single .woff2 component so it cannot traverse out of the embedded dir.
func (s *uiServer) handleEmoji(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/emoji/")
	if name == "" || strings.Contains(name, "/") || !strings.HasSuffix(name, ".woff2") {
		http.NotFound(w, r)
		return
	}
	data, err := emojiFS.ReadFile("ui_static/emoji/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

// handleSPA serves the single-page app. The real embedded UI replaces this stub.
func (s *uiServer) handleSPA(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// Inject the configured product name (browser tab title + login heading).
	page := bytes.ReplaceAll(spaHTML, []byte("__KLAX_UI_TITLE__"), []byte(html.EscapeString(s.d.cfg.GetUITitle())))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}
