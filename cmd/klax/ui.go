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

// Per-user event ring. Every broadcast event gets a monotonic seq and is retained
// here so a long-poll can return exactly what a client missed since its cursor
// ("<epoch>-<seq>"). Bounded by count and bytes; a cursor that predates the ring
// (overflow) gets `reload` and re-syncs from the transcript. The ring is the only
// delivery buffer — there is no per-connection state.
const (
	uiRingMaxItems = 512
	uiRingMaxBytes = 8 << 20
)

// uiPollHold is how long /api/poll holds a request open when there is nothing new
// before returning an empty batch (the client then re-polls). Well under typical
// proxy/browser request limits; a cut request is just re-issued from the same
// cursor (idempotent), so the exact value is not load-bearing.
const uiPollHold = 25 * time.Second

// uiMaxInflightPerUser bounds concurrently-held polls per user — cheap hygiene
// against a buggy client loop or an abusive token (replaces the SSE per-client
// cap/eviction). Excess polls get 429 + client backoff.
const uiMaxInflightPerUser = 32

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
	Type      string          `json:"type"` // sessions|turn_start|progress|final|error|notice|compact|user
	Seq       uint64          `json:"seq,omitempty"`      // monotonic id for client dedupe + unread; set by emitLocked
	Nonce     string          `json:"nonce,omitempty"`    // user-event: the sender's send nonce, so it skips its own echo
	Session   int64           `json:"session,omitempty"`
	TurnSeq   int64           `json:"turn_seq,omitempty"` // per-turn id: routes turn-scoped events to a turn's slot
	State     string          `json:"state,omitempty"`    // turn state this event sets: enq|run|done|err (read-model)
	Block     *uiBlock        `json:"block,omitempty"`    // progress/final/error: the answer block (with its stable id)
	Kind      string          `json:"kind,omitempty"` // progress: tool|narration
	Text      string          `json:"text,omitempty"`
	Markdown  string          `json:"markdown,omitempty"`
	Sessions  []uiSessionInfo `json:"sessions,omitempty"`
	Model     string          `json:"model,omitempty"`
	CtxUsed   int             `json:"ctx_used,omitempty"`
	CtxWindow int             `json:"ctx_window,omitempty"`
}

// uiUserForKey returns the canonical UI user for a session key, but ONLY for the
// canonical "user:<id>" form that UI clients (and mapped messenger DMs) resolve
// to. Raw messenger/group keys ("tg:123", "mx:...", group ids) return "" — a UI
// event must never reach a UI identity whose id merely collides with a raw chat
// suffix. uiEmit no-ops on the empty string.
func uiUserForKey(sk string) string {
	const p = "user:"
	if strings.HasPrefix(sk, p) {
		return sk[len(p):]
	}
	return ""
}

// ringItem is one retained event: its seq and the marshaled uiEvent JSON (which
// already carries `seq`). Stored as json.RawMessage so the poll handler can copy
// it into a response without unmarshal/remarshal.
type ringItem struct {
	seq  uint64
	data json.RawMessage
}

// uiHub retains events per canonical user for long-poll delivery. Every event gets
// a monotonic seq (under mu) and is kept in a bounded per-user ring; a poll reads
// events with seq>cursor. epoch is the process lifetime: a restart changes it, so a
// client with a stale-epoch cursor is told to reload. notify wakes held polls;
// inflight bounds concurrent held polls per user. There is no per-connection state.
type uiHub struct {
	mu       sync.Mutex
	epoch    int64
	seq      uint64
	ring     map[string][]ringItem    // per-user retained events
	ringSz   map[string]int           // per-user ring byte size
	notify   map[string]chan struct{} // per-user wake channel (closed-channel broadcast)
	inflight map[string]int           // per-user concurrently-held polls
}

func newUIHub() *uiHub {
	return &uiHub{
		epoch:    time.Now().UnixNano(), // unique per process so a restart is always detectable
		ring:     make(map[string][]ringItem),
		ringSz:   make(map[string]int),
		notify:   make(map[string]chan struct{}),
		inflight: make(map[string]int),
	}
}

// waitChan returns the per-user wake channel, creating it if absent. A poll grabs
// this BEFORE reading the ring so an emit between the read and the select closes
// the very channel it holds (lost-wakeup-safe).
func (h *uiHub) waitChan(user string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.notify[user]
	if ch == nil {
		ch = make(chan struct{})
		h.notify[user] = ch
	}
	return ch
}

// head returns the current global seq (the newest cursor position).
func (h *uiHub) head() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// enterPoll/leavePoll bound concurrently-held polls per user.
func (h *uiHub) enterPoll(user string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight[user] >= uiMaxInflightPerUser {
		return false
	}
	h.inflight[user]++
	return true
}

func (h *uiHub) leavePoll(user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight[user] > 0 {
		h.inflight[user]--
	}
	if h.inflight[user] == 0 {
		delete(h.inflight, user)
	}
}

// emitLocked assigns the next seq, stamps it into the payload (so the client can
// dedupe), appends the marshaled event to the user's ring (trimming to the caps),
// and wakes any held polls for that user. Caller holds h.mu.
func (h *uiHub) emitLocked(user string, ev uiEvent) {
	h.seq++
	ev.Seq = h.seq
	data, err := json.Marshal(ev)
	if err != nil {
		log.Printf("ui: marshal event: %v", err)
		return // seq is burned, but a single skipped seq is invisible to the cursor
	}
	items := append(h.ring[user], ringItem{seq: h.seq, data: data})
	sz := h.ringSz[user] + len(data)
	for len(items) > 1 && (len(items) > uiRingMaxItems || sz > uiRingMaxBytes) {
		sz -= len(items[0].data)
		items[0] = ringItem{} // drop the ref so it can be GC'd despite the backing array
		items = items[1:]
	}
	h.ring[user] = items
	h.ringSz[user] = sz
	if ch := h.notify[user]; ch != nil { // wake held polls; the next waiter makes a fresh channel
		close(ch)
		delete(h.notify, user)
	}
}

// collect gathers a user's events with seq > cursorSeq, the current head seq, and
// whether the cursor is uncoverable: epoch mismatch (daemon restarted) or it
// predates the retained ring (overflow). In both cases the client must reload from
// the transcript.
func (h *uiHub) collect(user string, cursorEpoch int64, cursorSeq uint64) (events []json.RawMessage, head uint64, reload bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	head = h.seq
	if cursorEpoch != h.epoch {
		return nil, head, true
	}
	items := h.ring[user]
	if len(items) > 0 && cursorSeq+1 < items[0].seq {
		return nil, head, true
	}
	for _, it := range items {
		if it.seq > cursorSeq {
			events = append(events, it.data)
		}
	}
	return events, head, false
}

// broadcast queues one event for a user and retains it for the next poll.
func (h *uiHub) broadcast(user string, ev uiEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.emitLocked(user, ev)
}

// broadcastAll queues one event for every user the hub knows about (has retained
// events for, or is currently polling) — daemon-wide banners (restart/update).
func (h *uiHub) broadcastAll(ev uiEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := make(map[string]struct{})
	for user := range h.ring {
		seen[user] = struct{}{}
	}
	for user := range h.inflight {
		seen[user] = struct{}{}
	}
	for user := range seen {
		h.emitLocked(user, ev)
	}
}

// uiNotifyAll pushes a notice to every connected UI client (any user).
func (d *daemon) uiNotifyAll(text string) {
	if d.uiHub == nil {
		return
	}
	d.uiHub.broadcastAll(uiEvent{Type: "notice", Text: text})
}

// uiEmit marshals and broadcasts one event to a user. No-op when the UI is off.
func (d *daemon) uiEmit(user string, ev uiEvent) {
	if d.uiHub == nil || user == "" {
		return
	}
	d.uiHub.broadcast(user, ev)
}

// broadcastSessions pushes the current tab-strip snapshot to the UI client(s)
// watching this session key. Cheap no-op when the UI is off or nobody watches.
func (d *daemon) broadcastSessions(sk string) {
	if d.uiHub == nil {
		return
	}
	d.uiEmit(uiUserForKey(sk), uiEvent{Type: "sessions", Sessions: d.sessionsSnapshot(sk)})
}

func (d *daemon) uiUserForChat(chatID string) string {
	return uiUserForKey(d.sessionKey(chatID))
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
	d.removeSessionStore(sk, created) // latch + delete the runner-owned store before dropping it
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
		log.Printf("ui: WARNING %q is not loopback — the bearer token travels in cleartext; keep ui_listen on 127.0.0.1 or front it with a TLS proxy", s.addr)
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
	mux.HandleFunc("/api/poll", s.handlePoll)
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/abort", s.handleAbort)
	mux.HandleFunc("/api/new", s.handleNew)
	mux.HandleFunc("/api/rename", s.handleRename)
	mux.HandleFunc("/api/close", s.handleClose)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/transcript", s.handleTranscript)
	mux.HandleFunc("/api/file", s.handleFile)
	mux.HandleFunc("/emoji/", s.handleEmoji)
	mux.HandleFunc("/", s.handleSPA)
	return mux
}

// auth resolves the bearer token (Authorization header) to a canonical user. Every
// UI request — including the long-poll — sets the header (fetch can), so there is
// no ?token= query path to widen token-in-URL leakage.
func (s *uiServer) auth(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	u, ok := s.tokens[strings.TrimPrefix(h, "Bearer ")]
	return u, ok
}

func (s *uiServer) chatID(user string) string { return uiPrefix + ":" + user }

// pollResponse is the /api/poll body: events since the cursor, the new cursor to
// send next, and a reload flag when the cursor is uncoverable.
type pollResponse struct {
	Epoch  int64             `json:"epoch"`
	Cursor string            `json:"cursor"`
	Events []json.RawMessage `json:"events,omitempty"`
	Reload bool              `json:"reload,omitempty"`
}

// parseCursor splits a "<epoch>-<seq>" poll cursor. ok=false on an absent/blank
// cursor (a fresh client).
func parseCursor(v string) (epoch int64, seq uint64, ok bool) {
	i := strings.IndexByte(v, '-')
	if i <= 0 {
		return 0, 0, false
	}
	e, err1 := strconv.ParseInt(v[:i], 10, 64)
	q, err2 := strconv.ParseUint(v[i+1:], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return e, q, true
}

func (s *uiServer) cursorString(seq uint64) string {
	return strconv.FormatInt(s.d.uiHub.epoch, 10) + "-" + strconv.FormatUint(seq, 10)
}

// handlePoll is the long-poll delivery endpoint. The client sends its cursor
// ("<epoch>-<seq>"); the server returns the user's events with seq>cursor, holding
// the request up to uiPollHold when there is nothing new (returning an empty batch
// on timeout, so the client re-polls). Ordinary request/response — no streaming,
// no heartbeat, no per-connection state — so any proxy handles it, and a cut
// request is just re-issued from the same cursor (idempotent, no loss).
func (s *uiServer) handlePoll(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Ensure the user has at least one session so the client's /api/sessions load
	// (cold start) has something to render.
	sk := s.d.sessionKey(s.chatID(user))
	s.d.ensureSessionWithCWD(sk, s.d.sessionCWD(s.chatID(user)))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	cursorEpoch, cursorSeq, hasCursor := parseCursor(r.URL.Query().Get("cursor"))

	// Fresh client (no cursor): just establish the cursor at the current head and
	// return immediately. Content comes from the client's /api/sessions + transcript
	// load; subsequent polls deliver everything after this head.
	if !hasCursor {
		s.writePoll(w, pollResponse{Epoch: s.d.uiHub.epoch, Cursor: s.cursorString(s.d.uiHub.head())})
		return
	}

	if !s.d.uiHub.enterPoll(user) {
		http.Error(w, "too many concurrent polls", http.StatusTooManyRequests)
		return
	}
	defer s.d.uiHub.leavePoll(user)

	deadline := time.NewTimer(uiPollHold)
	defer deadline.Stop()
	for {
		ch := s.d.uiHub.waitChan(user) // grab BEFORE collect (lost-wakeup-safe)
		events, head, reload := s.d.uiHub.collect(user, cursorEpoch, cursorSeq)
		if reload {
			s.writePoll(w, pollResponse{Epoch: s.d.uiHub.epoch, Cursor: s.cursorString(head), Reload: true})
			return
		}
		if len(events) > 0 {
			s.writePoll(w, pollResponse{Epoch: s.d.uiHub.epoch, Cursor: s.cursorString(head), Events: events})
			return
		}
		select {
		case <-ch:
			// woken by an emit for this user — re-collect
		case <-deadline.C:
			// nothing new within the hold — empty batch; head has no events for this
			// user in (cursor, head], so advancing the cursor to head loses nothing.
			s.writePoll(w, pollResponse{Epoch: s.d.uiHub.epoch, Cursor: s.cursorString(head)})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *uiServer) writePoll(w http.ResponseWriter, resp pollResponse) {
	_ = json.NewEncoder(w).Encode(resp)
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
	// Accept-during-drain: do NOT refuse here. enqueueToSession durably persists the
	// message and lets startup replay run it after the restart — the single
	// acceptance decision lives there, so a UI send during drain is not lost.
	// Cap the whole request body (attachments included) so a buggy or hostile
	// client cannot exhaust memory/disk while parsing.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	var (
		text          string
		nonce         string
		targetCreated int64
		attachments   []attachment
	)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		text = r.FormValue("text")
		nonce = r.FormValue("nonce")
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
			Nonce   string `json:"nonce"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		text = body.Text
		nonce = body.Nonce
		targetCreated = body.Session
	}
	// The UI always targets a specific tab; never silently fall back to the
	// active session the way the messenger paths do.
	if targetCreated <= 0 {
		http.Error(w, "a positive session is required", http.StatusBadRequest)
		return
	}
	// A UI send always carries a per-tab nonce; it's what lets the response bind the real
	// {seq,state} back to the optimistic echo, so a missing one is a malformed request.
	if nonce == "" {
		http.Error(w, "a send nonce is required", http.StatusBadRequest)
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
	// The accepted user message is echoed to every UI tab from the common accept
	// point (enqueueToSession), so a Telegram/MAX/VK DM shows up live too — not just
	// UI sends. The nonce rides along so THIS tab skips its own copy (it already
	// rendered it optimistically) while other tabs render it.
	if !s.d.handleInbound(Inbound{
		ChatID:        s.chatID(user),
		Text:          text,
		Attachments:   attachments,
		TargetCreated: targetCreated,
		Nonce:         nonce,
		RawMessage:    true, // the UI has no chat commands — "/"-text is a message
	}) {
		// Dropped after our entry checks (drain flipped in the window) — tell the
		// client so it rolls back the optimistic echo instead of leaving a ghost.
		http.Error(w, "klax перезапускается — попробуйте через минуту", http.StatusServiceUnavailable)
		return
	}
	// Return the durable {seq,state} so the sender binds its optimistic echo immediately;
	// the broadcast `user` event is the cross-tab/idempotent backstop. Looked up by the
	// send nonce from the just-written durable queue (a UI send always carries a nonce).
	sk := s.d.sessionKey(s.chatID(user))
	seq, state := int64(0), "enq"
	if log, _ := s.d.sessionStore(sk, targetCreated).InboundLog(); len(log) > 0 && nonce != "" {
		busy, newest := s.d.isSessionBusy(sk, targetCreated), newestRunSeq(log)
		for _, t := range log {
			if t.Nonce == nonce {
				seq, state = t.Seq, resolveTurnState(t.Last, busy, t.Seq == newest)
			}
		}
	}
	if seq == 0 {
		// nonce is required above, so an accepted send is always in the durable log — a 0
		// here means the log read failed; surface it rather than hand back a binding the
		// client can't use (the broadcast `user` event still re-shows the message).
		http.Error(w, "could not resolve accepted send", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Seq   int64  `json:"seq"`
		State string `json:"state"`
	}{Seq: seq, State: state})
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
	// Watermark = hub head BEFORE reading the transcript: every event with seq ≤ watermark
	// has flushed its content into the transcript (klax flushes before it emits), so the
	// client resumes its poll cursor here and applies only seq > watermark; any reload-
	// read/poll overlap is deduped by Block.id (REFACTOR_PLAN §A3).
	watermark := s.cursorString(s.d.uiHub.head())

	items, err := history.Load(backend, sess.ID, sess.CWD)
	if err != nil {
		log.Printf("ui: transcript load: %v", err)
		items = nil
	}
	// Pagination is BY TURN (top-level units), not flat items — a turn with hundreds of
	// tool blocks is one page unit, so its user message never scrolls off a page top.
	grouped := groupTurns(items)
	end := len(grouped)
	if before > 0 && int(before) < end {
		end = int(before)
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	queueTurns, _ := s.d.sessionStore(sk, created).InboundLog()
	turns := s.d.buildReadModel(sk, created, grouped[start:end], queueTurns, s.d.isSessionBusy(sk, created), start, before == 0)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Turns     []uiTurn `json:"turns"`
		More      bool     `json:"more"`
		Offset    int      `json:"offset"`
		Watermark string   `json:"watermark"`
	}{Turns: turns, More: start > 0, Offset: start, Watermark: watermark})
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

// handleSPA serves the single-page app shell at "/" and the SPA's ES modules / stylesheet
// at "/<name>.js" / "/<name>.css". /api/* and /emoji/ are more-specific routes and never
// reach here. Any other path 404s (no SPA-deep-link routing — the UI is one page).
func (s *uiServer) handleSPA(w http.ResponseWriter, r *http.Request) {
	if p := r.URL.Path; strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css") {
		s.serveModule(w, r, p)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// Inject the configured product name (browser tab title + login heading).
	page := bytes.ReplaceAll(spaHTML, []byte("__KLAX_UI_TITLE__"), []byte(html.EscapeString(s.d.cfg.GetUITitle())))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// serveModule serves one SPA ES module / stylesheet from the embedded ui_static dir. The
// name is constrained to a single path component (no traversal); like the shell and emoji
// font it needs no auth (the token gate is client-side). no-cache so a deploy's new
// modules are always picked up on the next reload.
func (s *uiServer) serveModule(w http.ResponseWriter, r *http.Request, p string) {
	name := strings.TrimPrefix(p, "/")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	data, err := moduleFS.ReadFile("ui_static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "text/javascript; charset=utf-8"
	if strings.HasSuffix(name, ".css") {
		ct = "text/css; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
