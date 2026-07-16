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
	"github.com/PiDmitrius/klax/internal/pathutil"
	"github.com/PiDmitrius/klax/internal/session"
)

// uiPrefix is the chatID/transport prefix for the web UI. A request authenticated
// as canonical user "alice" is handled as chatID "ui:alice", and sessionKey maps
// that to "user:alice" — the same key tg/mx DMs for alice resolve to, so the UI
// shares sessions with the messengers (cross-channel continuity).
const uiPrefix = "ui"

// Per-user NOTICE ring. CONTENT is delivered by the durable-tail poll (/api/tail) from the durable
// log; only transient notices are retained here, so a client catches the ones it missed since its
// notice cursor ("<epoch>-<seq>"). Bounded by count and bytes; a cursor that predates the ring
// (overflow) or a changed epoch (restart) resets it — notices are lost on restart by nature.
const (
	uiRingMaxItems = 512
	uiRingMaxBytes = 8 << 20
)

// uiPollHold is how long /api/tail holds a request open when there is nothing new before returning
// a session-strip refresh (the client then re-polls). Well under typical proxy/browser limits; a
// cut request is just re-issued from the same cursors (idempotent), so the value is not load-bearing.
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
	// Durable unread state. ReadThrough is the "<turn>.<block>"
	// watermark the client seeds its divider from; Unread is the exchange count the badge shows
	// for a tab the client has not loaded (a loaded tab counts precisely client-side).
	ReadThrough string `json:"read_through,omitempty"`
	Unread      int    `json:"unread,omitempty"`
}

// uiEvent is one server-sent event. The client multiplexes all tabs over a
// single stream and routes by Session (a session's Created).
type uiEvent struct {
	Type      string          `json:"type"`            // sessions|turn_start|context|progress|final|error|notice|user
	Seq       uint64          `json:"seq,omitempty"`   // monotonic id for client dedupe + unread; set by emitLocked
	Nonce     string          `json:"nonce,omitempty"` // user-event: the sender's send nonce, so it skips its own echo
	Session   int64           `json:"session,omitempty"`
	TurnSeq   int64           `json:"turn_seq,omitempty"` // per-turn id: routes turn-scoped events to a turn's slot
	State     string          `json:"state,omitempty"`    // turn state this event sets: enq|run|done|err (read-model)
	Block     *uiBlock        `json:"block,omitempty"`    // progress/final/error: the answer block (with its stable id)
	Kind      string          `json:"kind,omitempty"`     // progress: tool|narration
	Text      string          `json:"text,omitempty"`
	Time      string          `json:"time,omitempty"`
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

// uiHub wakes held tail-polls (notify) and retains only NOTICES per canonical user (seq under mu,
// bounded ring; the tail poll reads notices with seq>cursor). epoch is the process lifetime — a
// restart changes it, which the tail reports as `started` (the "klax обновился" banner) and which
// resets the notice cursor. inflight bounds concurrent held polls. There is no per-connection state.
// uiUnreadKey keys the read-model cache. readModelEntry caches a session's built rows by the
// transcript's AND queue's (mtime,size), so an unchanged session's rows — for the unread count AND
// the live tail — cost two os.Stat calls, not a transcript read + rebuild.
type uiUnreadKey struct {
	sk      string
	created int64
}
type readModelEntry struct {
	tMtime    time.Time
	tSize     int64
	qMtime    time.Time
	qSize     int64
	busy      bool // buildReadModel input NOT captured by the file stats — key on it so a busy⇄idle
	ctxWindow int  // flip / ctx-window change can never serve a stale cached read model
	rows      []uiTurn
}

type uiHub struct {
	mu       sync.Mutex
	epoch    int64
	seq      uint64
	ring     map[string][]ringItem           // per-user retained events
	ringSz   map[string]int                  // per-user ring byte size
	notify   map[string]chan struct{}        // per-user wake channel (closed-channel broadcast)
	inflight map[string]int                  // per-user concurrently-held polls
	known    map[string]struct{}             // every user that has ever polled — notice-broadcast target set
	acked    map[string]uint64               // newest notice seq acknowledged by each active browser tab
	ackWake  chan struct{}                   // closed/replaced whenever an acknowledgement advances
	clients  map[string]map[string]time.Time // recently polling browser tabs per canonical user
	sessRev  map[string]uint64               // per-user session-strip revision — bumped on every broadcastSessions
	rmMu     sync.Mutex                      // guards rm (read-model cache; separate from mu — off the poll hot path)
	rm       map[uiUnreadKey]readModelEntry
}

func newUIHub() *uiHub {
	return &uiHub{
		epoch:    time.Now().UnixNano(), // unique per process so a restart is always detectable
		ring:     make(map[string][]ringItem),
		ringSz:   make(map[string]int),
		notify:   make(map[string]chan struct{}),
		inflight: make(map[string]int),
		known:    make(map[string]struct{}),
		acked:    make(map[string]uint64),
		ackWake:  make(chan struct{}),
		clients:  make(map[string]map[string]time.Time),
		sessRev:  make(map[string]uint64),
		rm:       make(map[uiUnreadKey]readModelEntry),
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
	h.known[user] = struct{}{} // remember this user so notice broadcasts reach them even between polls
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

// emitLocked wakes any held tail-poll for the user, and — for NOTICES only — retains the event in
// the small per-user ring under a monotonic notice seq. Notices are the one transient broadcast
// left (command output / banners); CONTENT is recovered from the durable log by the tail, so it is
// NOT retained — a content emit just notifies. The notify ALWAYS fires (it is the tail-poll wake).
// Caller holds h.mu.
func (h *uiHub) emitLocked(user string, ev uiEvent) {
	if ev.Type == "notice" {
		h.seq++
		ev.Seq = h.seq
		if data, err := json.Marshal(ev); err == nil {
			items := append(h.ring[user], ringItem{seq: h.seq, data: data})
			sz := h.ringSz[user] + len(data)
			for len(items) > 1 && (len(items) > uiRingMaxItems || sz > uiRingMaxBytes) {
				sz -= len(items[0].data)
				items[0] = ringItem{} // drop the ref so it can be GC'd despite the backing array
				items = items[1:]
			}
			h.ring[user] = items
			h.ringSz[user] = sz
		} else {
			log.Printf("ui: marshal notice: %v", err)
		}
	}
	if ch := h.notify[user]; ch != nil { // wake held tail-polls; the next waiter makes a fresh channel
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

// broadcast queues one event for a user (only notices are retained; see emitLocked).
func (h *uiHub) broadcast(user string, ev uiEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.emitLocked(user, ev)
}

// poke wakes any held tail-poll for the user WITHOUT retaining anything — for CONTENT changes, which
// the tail recovers from the durable log. (Session-strip changes use bumpSessions.)
func (h *uiHub) poke(user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch := h.notify[user]; ch != nil {
		close(ch)
		delete(h.notify, user)
	}
}

// bumpSessions advances the user's session-strip revision and wakes held polls. The revision lets
// handleTail return a sessions-only change (rename/create/close/cross-tab read) immediately rather
// than holding until the timeout — content still has no retained state, but the strip does now.
func (h *uiHub) bumpSessions(user string) {
	if user == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessRev[user]++
	if ch := h.notify[user]; ch != nil {
		close(ch)
		delete(h.notify, user)
	}
}

// sessionsRev is the user's current session-strip revision (the client echoes its last-seen value
// back on the next tail so the server can detect a strip change with no content tail).
func (h *uiHub) sessionsRev(user string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessRev[user]
}

// broadcastAll queues one event for every user the hub knows about (has ever polled, has retained
// events, or is currently polling) — daemon-wide banners (restart/update). Since durable-tail
// removed retained content events, an active user often has no ring/inflight entry in the gap
// between two tail requests; `known` keeps the banner from being dropped for them.
func (h *uiHub) broadcastAll(ev uiEvent) map[string]uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := make(map[string]struct{})
	for user := range h.known {
		seen[user] = struct{}{}
	}
	for user := range h.ring {
		seen[user] = struct{}{}
	}
	for user := range h.inflight {
		seen[user] = struct{}{}
	}
	targets := make(map[string]uint64, len(seen))
	for user := range seen {
		h.emitLocked(user, ev)
		for client, seenAt := range h.clients[user] {
			if time.Since(seenAt) <= time.Minute {
				targets[uiClientKey(user, client)] = h.seq
			} else {
				delete(h.clients[user], client)
				delete(h.acked, uiClientKey(user, client))
			}
		}
	}
	return targets
}

func uiClientKey(user, client string) string { return user + "\x00" + client }

func (h *uiHub) observeClient(user, client, cursor string) {
	if client == "" {
		return
	}
	epoch, seq, ok := parseCursor(cursor)
	h.mu.Lock()
	defer h.mu.Unlock()
	clients := h.clients[user]
	if clients == nil {
		clients = make(map[string]time.Time)
		h.clients[user] = clients
	}
	for id, seenAt := range clients {
		if time.Since(seenAt) > time.Minute {
			delete(clients, id)
			delete(h.acked, uiClientKey(user, id))
		}
	}
	if _, exists := clients[client]; !exists && len(clients) >= uiMaxInflightPerUser*2 {
		return
	}
	clients[client] = time.Now()
	key := uiClientKey(user, client)
	if ok && epoch == h.epoch && seq > h.acked[key] {
		h.acked[key] = seq
		close(h.ackWake)
		h.ackWake = make(chan struct{})
	}
}

func (h *uiHub) waitAcknowledged(targets map[string]uint64, timeout time.Duration) {
	if len(targets) == 0 {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		h.mu.Lock()
		pending := false
		for user, seq := range targets {
			if h.acked[user] < seq {
				pending = true
				break
			}
		}
		wake := h.ackWake
		h.mu.Unlock()
		if !pending {
			return
		}
		select {
		case <-wake:
		case <-timer.C:
			return
		}
	}
}

// uiNotifyAll pushes a notice to every connected UI client (any user).
func (d *daemon) uiNotifyAll(text string) map[string]uint64 {
	if d.uiHub == nil {
		return nil
	}
	return d.uiHub.broadcastAll(uiEvent{Type: "notice", Text: text})
}

// uiEmit marshals and broadcasts one event to a user. No-op when the UI is off. Only NOTICES
// travel this path now; content and session-strip changes use uiPoke.
func (d *daemon) uiEmit(user string, ev uiEvent) {
	if d.uiHub == nil || user == "" {
		return
	}
	d.uiHub.broadcast(user, ev)
}

// uiPoke wakes a user's held tail-polls (a CONTENT change — delivered from the durable log by the
// tail). No-op when UI is off. Session-strip changes use broadcastSessions (which also bumps the rev).
func (d *daemon) uiPoke(user string) {
	if d.uiHub == nil || user == "" {
		return
	}
	d.uiHub.poke(user)
}

// broadcastSessions signals a session-strip change: it bumps the user's strip revision AND wakes
// held polls, so the next tail returns the fresh snapshot IMMEDIATELY (rev differs) instead of
// parking until the hold timeout when there is no content tail. No-op when the UI is off.
func (d *daemon) broadcastSessions(sk string) {
	if d.uiHub == nil {
		return
	}
	d.uiHub.bumpSessions(uiUserForKey(sk))
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
	n := len(sr.queue)
	if n > 0 && !sr.processing && !sr.runner.IsBusy() {
		n--
	}
	return n
}

// createUISessionAtomic creates a new UI session already fully configured. The session is VALIDATED
// and FORMED entirely outside the shared store, then inserted with a single Store.Add (one lock) and
// saved + announced once — so a concurrent /api/sessions can never observe a defaults or half-built
// placeholder, a rejected patch creates nothing at all, and a crash before the save leaves nothing.
// Returns the created session, or the settings error with NOTHING created.
func (d *daemon) createUISessionAtomic(sk, chatID string, patch uiSettingsPatch) (*session.Session, error) {
	def := d.scopeDefaults(sk)
	backend := resolveSessionBackend(nil, def, d.cfg.GetDefaultBackend())
	// Seed a message-less session from the scope defaults (what createSession would have produced),
	// then validate + apply the draft on it — all in memory, before the store is touched.
	sess := &session.Session{
		Name:          "session",
		Backend:       backend,
		ModelOverride: def.Model,
		ThinkOverride: def.Think,
		Sandbox:       effectiveSandboxMode(def, nil),
		ClaudeTTY:     def.ClaudeTTY && backend == "claude",
		CWD:           d.defaultSessionCWD(chatID, sk),
	}
	if draftHasFields(patch) {
		r, err := validateSettingsPatch(sess, backend, false, patch) // fresh session: never busy/locked
		if err != nil {
			return nil, err
		}
		applySettingsPatch(sess, r)
	}
	newDefaults := session.ScopeDefaults{
		Backend:   resolveSessionBackend(sess, def, d.cfg.GetDefaultBackend()),
		Model:     sess.ModelOverride,
		Think:     sess.ThinkOverride,
		Sandbox:   sess.Sandbox,
		ClaudeTTY: sess.ClaudeTTY,
		CWD:       sess.CWD,
	}
	created := d.store.AddWithDefaults(sk, sess, &newDefaults) // session + its template: one store commit
	d.saveStore()
	d.broadcastSessions(sk)
	return created, nil
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

// reorderSessions applies the tab strip's drag-and-drop order (by Created id) and
// pushes the updated strip. A no-op order change persists nothing.
func (d *daemon) reorderSessions(sk string, order []int64) bool {
	if !d.store.Reorder(sk, order) {
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
		return errors.New("Нельзя закрыть последнюю сессию")
	}
	idx, wasActive := -1, false
	for i, s := range sessions {
		if s.Created == created {
			idx, wasActive = i, s.Active
			break
		}
	}
	if idx == -1 {
		return errors.New("Сессия не найдена")
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
		// Unread answer-block count for a tab the client has not loaded (a loaded tab ignores this
		// and counts client-side). Stat-cached, so an unchanged session costs only an os.Stat.
		unread := d.sessionUnread(sk, s)
		out = append(out, uiSessionInfo{
			Created:     s.Created,
			Name:        s.Name,
			Active:      s.Active,
			Busy:        d.isSessionBusy(sk, s.Created),
			Queued:      d.queuedCount(sk, s.Created),
			Backend:     backend,
			Model:       model,
			CWD:         pathutil.TildePathsInText(s.CWD),
			Messages:    s.Messages,
			CtxUsed:     s.ContextUsed,
			CtxWindow:   s.ContextWindow,
			ReadThrough: fmt.Sprintf("%d.%d", s.ReadThroughTurn, s.ReadThroughBlock),
			Unread:      unread,
		})
	}
	return out
}

// sessionUnread returns a session's unread answer-block count for its tab badge — a loaded tab
// ignores this and counts client-side; this serves tabs the client has NOT loaded (finding B). It
// is cheap: readModel is cached, so this is a recount over cached rows.
func (d *daemon) sessionUnread(sk string, sess *session.Session) int {
	return unreadAfter(d.readModel(sk, sess), sess.ReadThroughTurn, sess.ReadThroughBlock)
}

// readModel builds a session's full read-model rows (durable queue ⋈ transcript) — the SAME rows
// the client renders, so server and client agree on one path (live delivery and reload converge on
// buildReadModel). It is a stat-keyed MEMOIZATION of buildReadModel, not a delivery channel: the key
// is EVERY input — the transcript's and queue's (mtime,size) plus the two non-file inputs (busy,
// ctxWindow) — so a hit is provably identical to a rebuild, and any change rebuilds once.
func (d *daemon) readModel(sk string, sess *session.Session) []uiTurn {
	st := d.sessionStore(sk, sess.Created)
	if st == nil {
		return nil
	}
	backend := sess.Backend
	if backend == "" {
		backend = "claude"
	}
	tm, ts, _ := history.Stat(backend, sess.ID, sess.CWD)
	qm, qs := st.QueueStat()
	busy := d.isSessionBusy(sk, sess.Created)
	cw := sess.ContextWindow
	key := uiUnreadKey{sk: sk, created: sess.Created}
	if d.uiHub != nil {
		h := d.uiHub
		h.rmMu.Lock()
		if e, ok := h.rm[key]; ok && e.tSize == ts && e.tMtime.Equal(tm) && e.qSize == qs && e.qMtime.Equal(qm) && e.busy == busy && e.ctxWindow == cw {
			h.rmMu.Unlock()
			return e.rows
		}
		h.rmMu.Unlock()
	}
	items, _ := history.Load(backend, sess.ID, sess.CWD)
	queueTurns, _ := st.InboundLog()
	rows := d.buildReadModel(sk, sess.Created, groupTurns(items), queueTurns, busy, 0, true, cw)
	if d.uiHub != nil {
		h := d.uiHub
		h.rmMu.Lock()
		h.rm[key] = readModelEntry{tMtime: tm, tSize: ts, qMtime: qm, qSize: qs, busy: busy, ctxWindow: cw, rows: rows}
		h.rmMu.Unlock()
	}
	return rows
}

// sessionTail is the live tail of a session's read model past a per-session
// (turn,block,state,trail,head) cursor — the rows the client merges to catch up. Empty when nothing
// is new past the cursor. [S3]
func (d *daemon) sessionTail(sk string, sess *session.Session, throughTurn int64, throughBlock int, throughState string, throughTrail int, head int64) []uiTurn {
	return tailFrom(d.readModel(sk, sess), throughTurn, throughBlock, throughState, throughTrail, head)
}

// watchRunTranscript pokes the user's tail whenever the active run's transcript FILE changes, so a
// block that lands in the file wakes the held poll even when no further stdout progress event
// follows (klax does not own the transcript write, and a stdout event can precede the file append).
// A brand-new session has no transcript address (id) at run start, so it waits for idKnown first.
// Polls history.Stat on a short tick until stop is closed (the run returns). No-op when UI is off.
func (d *daemon) watchRunTranscript(stop <-chan struct{}, idKnown <-chan string, backendName, cwd, sk, initialID string) {
	if d.uiHub == nil {
		return
	}
	id := initialID
	if id == "" {
		select {
		case id = <-idKnown:
		case <-stop:
			return
		}
	}
	user := uiUserForKey(sk)
	var lastM time.Time
	var lastS int64
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if m, s, ok := history.Stat(backendName, id, cwd); ok && (s != lastS || !m.Equal(lastM)) {
				lastM, lastS = m, s
				d.uiPoke(user)
			}
		}
	}
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

func (t *uiTransport) EditMessage(chatID, messageID, text, replyTo, format string) error {
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
	mux.HandleFunc("/api/tail", s.handleTail)
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/abort", s.handleAbort)
	mux.HandleFunc("/api/read", s.handleRead)
	mux.HandleFunc("/api/new", s.handleNew)
	mux.HandleFunc("/api/rename", s.handleRename)
	mux.HandleFunc("/api/reorder", s.handleReorder)
	mux.HandleFunc("/api/close", s.handleClose)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/system", s.handleSystem)
	mux.HandleFunc("/api/system/check", s.handleSystemCheck)
	mux.HandleFunc("/api/system/update", s.handleSystemUpdate)
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

// parseCursor splits a "<epoch>-<seq>" cursor — now only the transient-notice cursor. ok=false on
// an absent/blank cursor (a fresh client).
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

// --- durable-tail poll ---
// The live channel as a tail of the durable log: the client sends a per-session
// "<turn>.<block>.<state>.<trail>[.<head>]" cursor, the server returns the read-model rows past it
// (built by the SAME buildReadModel as a reload, so live and reload converge). No epoch/ring/reload
// for CONTENT — content is recovered from the durable log. `notice` stays a ring cursor: notices are
// transient broadcasts, not durable, so they keep a small retained buffer (the only surviving ring role).
type tailReq struct {
	Cursors map[string]string `json:"cursors"`  // created -> "<turn>.<block>.<state>.<trail>[.<head>]"
	Notice  string            `json:"notice"`   // ring cursor for transient notices
	SessRev uint64            `json:"sess_rev"` // last session-strip revision the client rendered
	Client  string            `json:"client"`   // page-lifetime id: restart notices are flushed to every active tab
}
type tailSessionData struct {
	Rows   []uiTurn `json:"rows"`
	Cursor string   `json:"cursor"` // new content cursor after these rows
}
type tailResp struct {
	Started  int64                      `json:"started"` // hub epoch — a change means the daemon restarted
	Startup  string                     `json:"startup"` // installed|started — canonical outcome of this process start
	Version  string                     `json:"version"`
	Sessions []uiSessionInfo            `json:"sessions"`
	Tails    map[string]tailSessionData `json:"tails,omitempty"`
	Notices  []string                   `json:"notices,omitempty"`
	Notice   string                     `json:"notice"`
	SessRev  uint64                     `json:"sess_rev"` // current session-strip revision (client echoes it back)
}

// parseBlockCursor splits a "<turn>.<block>.<state>.<trail>[.<head>]" content cursor (block may be
// -1 for a turn with no answer yet; the state code, trail, and head are absent on a legacy cursor).
// `trail` is the number of trailing non-durable rows after the last
// durable turn — it lets a standalone appended AFTER the last turn be delivered live exactly once.
// `head` is the newest durable turn the client has seen (only present when the cursor anchors on an
// OLDER still-running turn behind a queued one); it defaults to `turn` so a normal/legacy cursor is
// unchanged. Absent/blank ⇒ (0,-1,"",0,0) so a brand-new tab's first tail returns from the start.
func parseBlockCursor(v string) (turn int64, block int, state string, trail int, head int64) {
	parts := strings.SplitN(v, ".", 5)
	if len(parts) < 2 {
		return 0, -1, "", 0, 0
	}
	turn, _ = strconv.ParseInt(parts[0], 10, 64)
	block, _ = strconv.Atoi(parts[1])
	if len(parts) >= 3 {
		state = parts[2]
	}
	if len(parts) >= 4 {
		trail, _ = strconv.Atoi(parts[3])
	}
	head = turn // no separate head ⇒ the cursor anchor IS the newest turn seen
	if len(parts) >= 5 {
		head, _ = strconv.ParseInt(parts[4], 10, 64)
	}
	return turn, block, state, trail, head
}

// tailCursor is the position after applying `rows` — "<turn>.<block>.<state>.<trail>[.<head>]". The
// anchor (turn/block/state) is the OLDEST turn that is still UNSETTLED (enq/run): the cursor must not
// advance past it, so its later blocks and its completion are still delivered. When every turn is
// settled, the anchor is simply the last durable turn (the plain case). `trail` is the count of
// standalone rows after the last durable turn; `head` is the last durable turn's seq — emitted only
// when it is NEWER than the anchor (a queued turn sitting behind the still-running one), so the tail
// can tell a genuinely new turn from the already-seen queued one. State code + trail also advance the
// cursor on a pure enq→run transition or a trailing standalone with no new block.
func tailCursor(rows []uiTurn) string {
	var head, aTurn int64
	aBlock := -1
	aState := ""
	trail := 0
	anchored := false // locked onto the oldest unsettled turn — do not advance the anchor past it
	for _, t := range rows {
		if t.Role == "user" && t.Seq > 0 {
			head, trail = t.Seq, 0
			if !anchored {
				aTurn, aBlock, aState = t.Seq, len(t.Blocks)-1, t.State
				anchored = t.State == "enq" || t.State == "run"
			}
		} else {
			trail++ // a standalone / non-durable row after the last durable turn
		}
	}
	if anchored && aTurn != head {
		return fmt.Sprintf("%d.%d.%s.%d.%d", aTurn, aBlock, stateCode(aState), trail, head)
	}
	return fmt.Sprintf("%d.%d.%s.%d", aTurn, aBlock, stateCode(aState), trail)
}

func (s *uiServer) buildTail(user, sk string, req tailReq) tailResp {
	// Read the strip revision BEFORE the snapshot. A concurrent bumpSessions between the two must
	// never pair a NEWER rev with an OLDER snapshot: the client would ack a strip change it never
	// rendered, then stop re-requesting it (its next rev matches the server's) and stay stale until a
	// later wake/timeout. Since bumpSessions mutates the store BEFORE incrementing the rev, a snapshot
	// taken after `rev` reflects at least rev's state — so resp.SessRev is never ahead of Sessions,
	// and any bump after `rev` instead closes the wake channel and drives a rebuild.
	rev := s.d.uiHub.sessionsRev(user)
	resp := tailResp{Started: s.d.uiHub.epoch, Startup: s.d.startupKind, Version: version, Sessions: s.d.sessionsSnapshot(sk), SessRev: rev}
	for createdStr, cur := range req.Cursors {
		created, err := strconv.ParseInt(createdStr, 10, 64)
		if err != nil {
			continue
		}
		sess := s.d.store.Get(sk, created)
		if sess == nil {
			continue
		}
		ct, cb, cs, ctr, chd := parseBlockCursor(cur)
		rows := s.d.sessionTail(sk, sess, ct, cb, cs, ctr, chd)
		if len(rows) == 0 {
			continue
		}
		if resp.Tails == nil {
			resp.Tails = make(map[string]tailSessionData)
		}
		resp.Tails[createdStr] = tailSessionData{Rows: rows, Cursor: tailCursor(rows)}
	}
	// Notices ride the ring (the one transient broadcast left). A reload here just resets the
	// notice cursor — notices are lost on restart by nature, no replay needed.
	noticeEpoch, noticeSeq, _ := parseCursor(req.Notice)
	events, head, reload := s.d.uiHub.collect(user, noticeEpoch, noticeSeq)
	if !reload {
		for _, raw := range events {
			var ev uiEvent
			if json.Unmarshal(raw, &ev) == nil && ev.Type == "notice" {
				resp.Notices = append(resp.Notices, ev.Text)
			}
		}
	}
	resp.Notice = s.cursorString(head)
	return resp
}

// handleTail is the durable-tail long-poll: hold on the per-user notify until any watched session
// has content past its cursor (or a notice), then return the tail rows + refreshed session strip.
func (s *uiServer) handleTail(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	s.d.ensureSessionWithCWD(sk, s.d.sessionCWD(s.chatID(user)))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	var req tailReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if !s.d.uiHub.enterPoll(user) {
		http.Error(w, "Too many concurrent polls", http.StatusTooManyRequests)
		return
	}
	defer s.d.uiHub.leavePoll(user)
	s.d.uiHub.observeClient(user, req.Client, req.Notice)

	deadline := time.NewTimer(uiPollHold)
	defer deadline.Stop()
	ch := s.d.uiHub.waitChan(user) // grab BEFORE building (lost-wakeup-safe)
	resp := s.buildTail(user, sk, req)
	// Return immediately on new content, a notice, OR a session-strip change the client hasn't seen
	// (resp.SessRev past the client's last-rendered rev). The rev catches a rename/create/close/
	// cross-tab-read whose wake was lost because this request arrived just after it — without it,
	// such a strip-only change would park here until the hold timeout (up to uiPollHold).
	if len(resp.Tails) > 0 || len(resp.Notices) > 0 || resp.SessRev != req.SessRev {
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	// Nothing new yet — hold until ANY emit for this user (content, a notice, a session-strip
	// change such as a cross-tab POST /api/read), then return a FRESH build so the snapshot and
	// any tails land promptly; on timeout return the current strip so badges never go stale.
	select {
	case <-ch:
		_ = json.NewEncoder(w).Encode(s.buildTail(user, sk, req))
	case <-deadline.C:
		_ = json.NewEncoder(w).Encode(resp)
	case <-r.Context().Done():
	}
}

func (s *uiServer) handleSend(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
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
			http.Error(w, "Bad multipart", http.StatusBadRequest)
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
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		text = body.Text
		nonce = body.Nonce
		targetCreated = body.Session
	}
	// The UI always targets a specific tab; never silently fall back to the
	// active session the way the messenger paths do.
	if targetCreated <= 0 {
		http.Error(w, "A positive session is required", http.StatusBadRequest)
		return
	}
	// A UI send always carries a per-tab nonce for idempotency and traceability, so a
	// missing one is a malformed request.
	if nonce == "" {
		http.Error(w, "A send nonce is required", http.StatusBadRequest)
		return
	}
	// Refuse a send to a tab whose session no longer exists (e.g. closed from
	// another client): enqueue would drop it with only an SSE notice while we'd still
	// answer success. A 404 lets the client restore the composer. (Mirrors handleAbort's
	// existence check.)
	if s.d.store.Get(s.d.sessionKey(s.chatID(user)), targetCreated) == nil {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}
	// The accepted user message is echoed to every UI tab from the common accept
	// point (enqueueToSession), so a Telegram/MAX/VK DM shows up live too — not just
	// UI sends. The web client does not render a local echo; the server event is the
	// first visible copy.
	if !s.d.handleInbound(Inbound{
		ChatID:        s.chatID(user),
		Text:          text,
		Attachments:   attachments,
		TargetCreated: targetCreated,
		Nonce:         nonce,
		RawMessage:    true, // the UI has no chat commands — "/"-text is a message
	}) {
		// Dropped after our entry checks (drain flipped in the window) — tell the
		// client so it restores the composer instead of silently losing the draft.
		http.Error(w, "Сервис перезапускается — попробуйте через минуту", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *uiServer) handleAbort(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Session int64 `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if body.Session <= 0 {
		http.Error(w, "A positive session is required", http.StatusBadRequest)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	if s.d.store.Get(sk, body.Session) == nil {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}
	s.d.abortSession(sk, body.Session, false)
	w.WriteHeader(http.StatusNoContent)
}

// handleRead persists the durable per-session read-through watermark — the (turn_seq, block index)
// the client reports as it reads down. The client pushes
// it PROACTIVELY (on read-settle and on tab-hide), so the read state is durable before the tab can
// go away and thus survives reload + daemon restart. The watermark only ever RAISES (monotonic),
// so a stale or out-of-order report can never un-read messages. Persists only when it actually
// moved. The unread COUNT the badge shows is computed separately (badge-granularity open question).
func (s *uiServer) handleRead(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Session int64 `json:"session"`
		Turn    int64 `json:"turn"`
		Block   int   `json:"block"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if body.Session <= 0 {
		http.Error(w, "A positive session is required", http.StatusBadRequest)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	if s.d.store.Get(sk, body.Session) == nil {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}
	raised := false
	s.d.store.UpdateSession(sk, body.Session, func(cur *session.Session) {
		raised = raiseReadThrough(cur, body.Turn, body.Block)
	})
	if raised {
		s.d.saveStore()
		s.d.broadcastSessions(sk) // push the new read_through/unread to this user's other tabs/devices
	}
	w.WriteHeader(http.StatusNoContent)
}

// raiseReadThrough advances a session's durable read watermark to (turn, block) only when that is
// strictly ahead of the stored one — a later turn, or the same turn with a further block — so a
// duplicate or out-of-order report never regresses it. Returns whether it moved (⇒ worth saving).
func raiseReadThrough(cur *session.Session, turn int64, block int) bool {
	if turn < cur.ReadThroughTurn || (turn == cur.ReadThroughTurn && block <= cur.ReadThroughBlock) {
		return false
	}
	cur.ReadThroughTurn = turn
	cur.ReadThroughBlock = block
	return true
}

func (s *uiServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
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
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Optional initial settings from the "new session" draft dialog: the tab strip
	// now defers creation until the draft is confirmed, sending the chosen fields
	// here so the session is born configured. An empty body keeps the old behaviour.
	var patch uiSettingsPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil && err != io.EOF {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	// Atomic create: validate + configure the session, then a SINGLE save + broadcast. A rejected
	// draft (e.g. an inaccessible cwd) creates nothing and returns the real reason; nothing external
	// ever sees a defaults placeholder, and a crash before the save leaves no half-built session.
	sess, err := s.d.createUISessionAtomic(sk, s.chatID(user), patch)
	if err != nil {
		status := http.StatusBadRequest
		if ue, ok := err.(*uiErr); ok {
			status = ue.status
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Created int64 `json:"created"`
	}{sess.Created})
}

func (s *uiServer) handleRename(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Session int64  `json:"session"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if body.Session <= 0 || strings.TrimSpace(body.Name) == "" {
		http.Error(w, "Session and name are required", http.StatusBadRequest)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	if !s.d.renameSession(sk, body.Session, body.Name) {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *uiServer) handleReorder(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Order []int64 `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	s.d.reorderSessions(sk, body.Order)
	w.WriteHeader(http.StatusNoContent)
}

func (s *uiServer) handleClose(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Session int64 `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if body.Session <= 0 {
		http.Error(w, "A positive session is required", http.StatusBadRequest)
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
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
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
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}
	backend := sess.Backend
	if backend == "" {
		backend = "claude"
	}
	// Watermark = hub head BEFORE reading the transcript: every event with seq ≤ watermark
	// has flushed its content into the transcript (klax flushes before it emits), so the
	// client resumes its poll cursor here and applies only seq > watermark; any reload-
	// read/poll overlap is deduped by Block.id.
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
	turns := s.d.buildReadModel(sk, created, grouped[start:end], queueTurns, s.d.isSessionBusy(sk, created), start, before == 0, sess.ContextWindow)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Turns       []uiTurn `json:"turns"`
		More        bool     `json:"more"`
		Offset      int      `json:"offset"`
		Watermark   string   `json:"watermark"`
		ReadThrough string   `json:"read_through"` // the tab seeds its unread divider from this
	}{Turns: turns, More: start > 0, Offset: start, Watermark: watermark, ReadThrough: fmt.Sprintf("%d.%d", sess.ReadThroughTurn, sess.ReadThroughBlock)})
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
