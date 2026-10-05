package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/history"
	"github.com/PiDmitrius/klax/internal/inbound"
	"github.com/PiDmitrius/klax/internal/pathutil"
	"github.com/PiDmitrius/klax/internal/session"
)

// uiPrefix is the chatID/transport prefix for the web UI. A request authenticated
// as canonical user "alice" is handled as chatID "ui:alice", and sessionKey maps
// that to "user:alice" — the same key tg/mx DMs for alice resolve to, so the UI
// shares sessions with the messengers (cross-channel continuity).
const uiPrefix = "ui"

// uiPollHold is how long /api/changes holds a request open when there is nothing new. Well under
// typical proxy/browser limits; a cut request is just re-issued from the same cursor (idempotent).
var uiPollHold = 25 * time.Second

// uiMaxInflightPerUser bounds concurrently-held polls per user — cheap hygiene
// against a buggy client loop or an abusive token. Excess polls get 429 + client backoff.
const uiMaxInflightPerUser = 32

// uiSessionInfo is one tab in the strip.
type uiSessionInfo struct {
	ReadOnly  bool   `json:"read_only"`
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
	// Durable unread state. ReadThrough is the "<turn>.<block>" watermark the client seeds its
	// divider from; Unread is the count the badge shows for a session the client has not loaded
	// (a loaded one counts its replicated rows).
	ReadThrough string `json:"read_through,omitempty"`
	Unread      int    `json:"unread,omitempty"`
	// Groups the session belongs to. The client filters the strip and derives the whole group list
	// from this — there is no group registry and no /api/groups.
	Groups []string `json:"groups,omitempty"`
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

// uiHub owns the live channel: per-user wake channels for held polls, the per-user sync state and
// event rings (uisync.go), and the read-model cache. epoch is a random id of the process — a
// restart changes it, so a client cursor from another process resyncs. polls are the held requests.
// uiUnreadKey keys the read-model cache. readModelEntry caches a session's built rows by the
// transcript's AND queue's (mtime,size), so an unchanged session's rows cost two os.Stat calls, not a
// transcript read + rebuild.
type uiUnreadKey struct {
	sk      string
	created int64
}
type readModelEntry struct {
	tMtime   time.Time
	tSize    int64
	qMtime   time.Time
	qSize    int64
	busy     bool // buildReadModel input NOT captured by the file stats — a busy⇄idle flip rebuilds
	rows     []uiTurn
	memo     map[rowKey]uiTurn
	gen      uint64 // transcript index generation the memo was built from
	build    uint64 // identifies this build; the detector skips a session whose build it published
	degraded bool   // a file link could not be published yet; rebuilt once uiPollHold has passed
	builtAt  time.Time
}

type uiHub struct {
	mu        sync.Mutex
	epoch     string
	seq       uint64
	notify    map[string]chan struct{} // per-user wake channel (closed-channel broadcast)
	users     map[string]*uiUserSync
	polls     map[*uiPoll]struct{}
	pollsWake chan struct{}
	rmMu      sync.Mutex // guards rm and rmBuild (separate from mu — off the poll hot path)
	rm        map[uiUnreadKey]readModelEntry
	rmBuild   uint64
}

// newEpoch returns 8 random characters of [A-Za-z0-9]: compared only for equality, never parsed.
func newEpoch() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b [8]byte
	rand.Read(b[:])
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:])
}

func newUIHub() *uiHub {
	return &uiHub{
		epoch:     newEpoch(),
		notify:    make(map[string]chan struct{}),
		users:     make(map[string]*uiUserSync),
		polls:     make(map[*uiPoll]struct{}),
		pollsWake: make(chan struct{}),
		rm:        make(map[uiUnreadKey]readModelEntry),
	}
}

// waitChan returns the per-user wake channel, creating it if absent. A poll grabs this BEFORE
// detecting so a change between the detection and the select closes the very channel it holds.
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

// uiNotifyAll pushes a notice to every UI user and returns its seq. No-op when the UI is off.
func (d *daemon) uiNotifyAll(text string) uint64 {
	if d.uiHub == nil {
		return 0
	}
	return d.uiHub.noticeAll(text)
}

// uiNotice pushes a transient notice to one user. No-op when the UI is off.
func (d *daemon) uiNotice(user, text string) {
	if d.uiHub == nil || user == "" {
		return
	}
	d.uiHub.notice(user, text)
}

// uiPoke wakes a user's held polls so they run the detector. No-op when UI is off.
func (d *daemon) uiPoke(user string) {
	if d.uiHub == nil || user == "" {
		return
	}
	d.uiHub.mu.Lock()
	d.uiHub.wakeLocked(user)
	d.uiHub.mu.Unlock()
}

// broadcastSessions signals a session-strip change to the user's held polls.
func (d *daemon) broadcastSessions(sk string) {
	d.uiPoke(uiUserForKey(sk))
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
		r, err := d.validateSettingsPatch(sess, backend, false, patch) // fresh session: never busy/locked
		if err != nil {
			return nil, err
		}
		applySettingsPatch(sess, r)
	}
	newDefaults := session.ScopeDefaults{
		Backend:      resolveSessionBackend(sess, def, d.cfg.GetDefaultBackend()),
		Model:        sess.ModelOverride,
		Think:        sess.ThinkOverride,
		Sandbox:      sess.Sandbox,
		ClaudeTTY:    sess.ClaudeTTY,
		CWD:          sess.CWD,
		GroupMode:    def.GroupMode,
		GroupVerbose: def.GroupVerbose,
	}
	created, err := d.store.AddPersisted(sk, sess, &newDefaults)
	if err != nil {
		return nil, &uiErr{http.StatusInternalServerError, "Не удалось сохранить сессию"}
	}
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
	d.store.DeleteCreated(sk, created)
	d.removeSessionStore(sk, created) // latch + delete the runner-owned store before dropping it
	d.dropRunner(sk, created)
	if wasActive {
		d.store.Switch(sk, 0) // promote the first remaining session
	}
	d.saveStore()
	d.broadcastSessions(sk)
	return nil
}

// sessionsSnapshot builds the tab strip of one access role from the sessions and read-model rows
// the detector took in one pass, so the strip and the published groups describe the same build.
func (d *daemon) sessionsSnapshot(sk string, sessions []*session.Session, rows map[int64][]uiTurn, readOnly bool) []uiSessionInfo {
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
		readTurn, readBlock := s.ReadThrough(readOnly)
		out = append(out, uiSessionInfo{
			Created:     s.Created,
			ReadOnly:    readOnly,
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
			ReadThrough: fmt.Sprintf("%d.%d", readTurn, readBlock),
			Unread:      unreadAfter(rows[s.Created], readTurn, readBlock),
			Groups:      s.Groups,
		})
	}
	return out
}

// readModelBuild returns a session's full read-model rows (durable queue ⋈ transcript) — the SAME
// rows the client renders — and the build they come from. It is a stat-keyed MEMOIZATION of
// buildReadModel, not a delivery channel: the key is EVERY input — the transcript's and queue's
// (mtime,size) plus busy — so a hit is identical to a rebuild, and any change rebuilds once, reusing
// the rows of turns it did not touch (rowMemo). A build with a not-yet-publishable file link is
// retried once uiPollHold has passed. ok is false when the history could not be read; nothing is
// cached then.
func (d *daemon) readModelBuild(sk string, sess *session.Session) (rows []uiTurn, build uint64, ok bool) {
	st := d.sessionStore(sk, sess.Created)
	if st == nil {
		return nil, 0, false
	}
	backend := sess.Backend
	if backend == "" {
		backend = "claude"
	}
	tm, ts, _ := history.Stat(backend, sess.ID, sess.CWD)
	qm, qs := st.QueueStat()
	busy := d.isSessionBusy(sk, sess.Created)
	key := uiUnreadKey{sk: sk, created: sess.Created}
	var prev readModelEntry
	if d.uiHub != nil {
		h := d.uiHub
		h.rmMu.Lock()
		e, hit := h.rm[key]
		if hit && e.tSize == ts && e.tMtime.Equal(tm) && e.qSize == qs && e.qMtime.Equal(qm) && e.busy == busy &&
			!(e.degraded && time.Since(e.builtAt) >= uiPollHold) {
			h.rmMu.Unlock()
			return e.rows, e.build, true
		}
		prev = e
		h.rmMu.Unlock()
	}
	items, gen, err := history.LoadGeneration(backend, sess.ID, sess.CWD)
	if err != nil {
		log.Printf("ui: transcript load (%s/%d): %v", sk, sess.Created, err)
		return prev.rows, prev.build, false
	}
	memo := &rowMemo{}
	if prev.gen == gen {
		memo.prev = prev.memo
	}
	queueTurns, _ := st.InboundLog()
	rows = d.buildReadModel(sk, sess.Created, groupTurns(items), queueTurns, busy, memo)
	if d.uiHub != nil {
		h := d.uiHub
		h.rmMu.Lock()
		h.rmBuild++
		build = h.rmBuild
		h.rm[key] = readModelEntry{tMtime: tm, tSize: ts, qMtime: qm, qSize: qs, busy: busy, rows: rows, memo: memo.next, gen: gen,
			build: build, degraded: memo.degraded, builtAt: time.Now()}
		h.rmMu.Unlock()
	}
	return rows, build, true
}

// dropReadModel forgets a closed session's cached rows.
func (d *daemon) dropReadModel(sk string, created int64) {
	if d.uiHub == nil {
		return
	}
	d.uiHub.rmMu.Lock()
	delete(d.uiHub.rm, uiUnreadKey{sk: sk, created: created})
	d.uiHub.rmMu.Unlock()
}

// watchRunTranscript pokes the user's held polls whenever the active run's transcript FILE changes, so a
// block that lands in the file wakes the held poll even when no further stdout progress event
// follows (klax does not own the transcript write, and a stdout event can precede the file append).
// A brand-new session has no transcript address (id) at run start, so it waits for idKnown first.
// Polls history.Stat on a short tick until stop is closed (the run returns). No-op when UI is off.
func (d *daemon) watchRunTranscript(stop <-chan struct{}, idKnown <-chan string, backendName, cwd, sk string, created int64, initialID string) {
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
				d.reconcileBindings(sk, created, backendName, id, cwd)
				d.uiPoke(user)
			}
		}
	}
}

type uiAccess struct {
	User     string `json:"user"`
	ReadOnly bool   `json:"read_only"`
}

func buildUITokens(users []config.UserIdentity) (map[string]uiAccess, error) {
	tokens := make(map[string]uiAccess)
	for _, u := range users {
		for _, entry := range []struct {
			token    string
			readOnly bool
		}{{u.UIToken, false}, {u.UIReadToken, true}} {
			if entry.token == "" {
				continue
			}
			if u.ID == "" {
				return nil, fmt.Errorf("ui: token owner has an empty id")
			}
			if _, dup := tokens[entry.token]; dup {
				return nil, fmt.Errorf("ui: duplicate access token in config")
			}
			tokens[entry.token] = uiAccess{User: u.ID, ReadOnly: entry.readOnly}
		}
	}
	return tokens, nil
}

// uiTransport adapts the web UI to transport.Transport so every existing reply
// path (sendMessage/sendPlain, command output, errors) reaches the UI as a
// notice without touching those call sites. It is registered in
// d.transports["ui"] but deliberately excluded from /transports (it is not a
// pollable messenger).
type uiTransport struct {
	d *daemon
}

func (t *uiTransport) SendMessage(chatID, text, replyTo, format string) error {
	t.d.uiNotice(t.d.uiUserForChat(chatID), text)
	return nil
}

func (t *uiTransport) SendMessageReturnID(chatID, text, replyTo, format string) (string, error) {
	t.d.uiNotice(t.d.uiUserForChat(chatID), text)
	return "ui-notice", nil
}

func (t *uiTransport) EditMessage(chatID, messageID, text, replyTo, format string) error {
	t.d.uiNotice(t.d.uiUserForChat(chatID), text)
	return nil
}

// uiServer is the HTTP Source. It binds 127.0.0.1 (per config), serves the
// SPA and the JSON API, and authenticates every request by bearer token.
type uiServer struct {
	d        *daemon
	addr     string
	tokens   map[string]uiAccess // token -> user and access role
	sendTest uiSendTest
}

func (s *uiServer) Name() string { return uiPrefix }

func (s *uiServer) Run(ctx context.Context) {
	s.sendTest.enabled = os.Getenv("KLAX_UI_SEND_TEST") == "1"
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
	mux.HandleFunc("/api/auth", s.handleAuth)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/changes", s.handleChanges)
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/abort", s.handleAbort)
	mux.HandleFunc("/api/cancel", s.handleCancel)
	mux.HandleFunc("/api/read", s.handleRead)
	mux.HandleFunc("/api/new", s.handleNew)
	mux.HandleFunc("/api/rename", s.handleRename)
	mux.HandleFunc("/api/reorder", s.handleReorder)
	mux.HandleFunc("/api/close", s.handleClose)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/models/refresh", s.handleModelsRefresh)
	mux.HandleFunc("/api/system", s.handleSystem)
	mux.HandleFunc("/api/system/check", s.handleSystemCheck)
	mux.HandleFunc("/api/system/update", s.handleSystemUpdate)
	mux.HandleFunc("/api/transcript", s.handleTranscript)
	mux.HandleFunc("/api/file", s.handleFile)
	mux.HandleFunc("/emoji/", s.handleEmoji)
	mux.HandleFunc("/", s.handleSPA)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/file" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			s.handleFile(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			access, ok := s.access(r)
			if !ok {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			if access.ReadOnly && !readerRequest(r) {
				writeAPIError(w, apiFailure("read-only"))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// auth resolves the bearer token (Authorization header) to a canonical user. Every
// UI request — including the long-poll — sets the header (fetch can), so there is
// no ?token= query path to widen token-in-URL leakage.
func (s *uiServer) access(r *http.Request) (uiAccess, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return uiAccess{}, false
	}
	access, ok := s.tokens[strings.TrimPrefix(h, "Bearer ")]
	return access, ok
}

func (s *uiServer) auth(r *http.Request) (string, bool) {
	access, ok := s.access(r)
	return access.User, ok
}

func (s *uiServer) readOnly(r *http.Request) bool {
	access, _ := s.access(r)
	return access.ReadOnly
}

// Readers may change only their own watermark; all other routes are denied by default.
func readerRequest(r *http.Request) bool {
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/api/auth", "/api/state", "/api/settings", "/api/system", "/api/transcript", "/api/file":
			return true
		}
	}
	return r.Method == http.MethodPost && (r.URL.Path == "/api/changes" || r.URL.Path == "/api/read")
}

func (s *uiServer) handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	access, _ := s.access(r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(access)
}

func (s *uiServer) chatID(user string) string { return uiPrefix + ":" + user }

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
		text                string
		nonce               string
		returnOn            = "queued"
		nonceRaw, returnRaw json.RawMessage
		targetCreated       int64
		attachments         []attachment
	)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "Bad multipart", http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		text = r.FormValue("text")
		if values, ok := r.MultipartForm.Value["nonce"]; ok {
			if len(values) != 1 {
				http.Error(w, "Invalid nonce", http.StatusBadRequest)
				return
			}
			nonceRaw, _ = json.Marshal(values[0])
		}
		if values, ok := r.MultipartForm.Value["return_on"]; ok {
			if len(values) != 1 {
				http.Error(w, "Invalid return_on", http.StatusBadRequest)
				return
			}
			returnRaw, _ = json.Marshal(values[0])
		}
		targetCreated, _ = strconv.ParseInt(r.FormValue("session"), 10, 64)
		for _, fh := range r.MultipartForm.File["files"] {
			f, err := fh.Open()
			if err != nil {
				http.Error(w, "Не удалось прочитать вложение", http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				http.Error(w, "Не удалось прочитать вложение", http.StatusBadRequest)
				return
			}
			attachments = append(attachments, attachment{filename: fh.Filename, data: data})
		}
	} else {
		var body struct {
			Session  int64           `json:"session"`
			Text     string          `json:"text"`
			Nonce    json.RawMessage `json:"nonce"`
			ReturnOn json.RawMessage `json:"return_on"`
		}
		if err := decodeAPIRequest(r.Body, &body, false); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		text = body.Text
		nonceRaw, returnRaw = body.Nonce, body.ReturnOn
		targetCreated = body.Session
	}
	// The UI always targets a specific tab; never silently fall back to the
	// active session the way the messenger paths do.
	if targetCreated <= 0 {
		http.Error(w, "A positive session is required", http.StatusBadRequest)
		return
	}
	var parseErr *apiError
	nonce, parseErr = optionalNonemptyString(nonceRaw, "nonce", "")
	if parseErr != nil {
		writeAPIError(w, parseErr)
		return
	}
	returnOn, parseErr = optionalNonemptyString(returnRaw, "return_on", "queued")
	if parseErr != nil {
		writeAPIError(w, parseErr)
		return
	}
	if returnOn != "queued" && returnOn != "start" && returnOn != "finish" {
		writeAPIError(w, &apiError{Code: "invalid-return-on", Message: "Неизвестное значение return_on", status: http.StatusBadRequest})
		return
	}
	if nonce == "" {
		var value [16]byte
		if _, err := rand.Read(value[:]); err != nil {
			writeAPIError(w, apiFailure("enqueue-failed"))
			return
		}
		nonce = hex.EncodeToString(value[:])
	}
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		writeAPIError(w, &apiError{Code: "empty-message", Message: "Сообщение пусто", status: http.StatusBadRequest})
		return
	}
	if !s.requireSession(w, s.d.sessionKey(s.chatID(user)), targetCreated) {
		return
	}
	if s.sendTest.intercept(w, r, user) {
		return
	}
	// The accepted user message is echoed to every UI tab from the common accept
	// point (enqueueToSession), so a Telegram/MAX/VK DM shows up live too — not just
	// UI sends. The web client does not render a local echo; the server event is the
	// first visible copy.
	admission := &sendAdmission{}
	if !s.d.handleInbound(Inbound{
		admission:     admission,
		ChatID:        s.chatID(user),
		Text:          text,
		Attachments:   attachments,
		TargetCreated: targetCreated,
		Nonce:         nonce,
		RawMessage:    true, // the UI has no chat commands — "/"-text is a message
		Origin: inbound.Origin{
			Transport: "ui",
			Chat:      inbound.Chat{ID: user, Type: "private"},
			Message:   inbound.Message{ID: nonce},
			Sender:    inbound.Sender{ID: user, Username: user},
		},
	}) {
		if admission.err != nil {
			writeAPIError(w, admission.err)
			return
		}
		// Dropped after our entry checks (drain flipped in the window) — tell the
		// client so it restores the composer instead of silently losing the draft.
		http.Error(w, "Сервис перезапускается — попробуйте через минуту", http.StatusServiceUnavailable)
		return
	}
	if returnOn != "queued" {
		awaitTurn(w, r, admission.completion, returnOn)
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

// handleCancel drops one queued message of a session; 409 not-queued once it has started or settled.
func (s *uiServer) handleCancel(w http.ResponseWriter, r *http.Request) {
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
		Seq     int64 `json:"seq"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Session <= 0 || body.Seq <= 0 {
		http.Error(w, "A positive session and seq are required", http.StatusBadRequest)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	if !s.requireSession(w, sk, body.Session) {
		return
	}
	found, err := s.d.cancelQueued(sk, body.Session, body.Seq)
	if err != nil {
		log.Printf("durable MarkErr cancelled (%s/%d): %v", sk, body.Session, err)
		writeAPIError(w, apiFailure("cancel-failed"))
		return
	}
	if !found {
		writeAPIError(w, apiFailure("not-queued"))
		return
	}
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
		raised = cur.AdvanceReadThrough(s.readOnly(r), body.Turn, body.Block)
	})
	if raised {
		s.d.saveStore()
		s.d.broadcastSessions(sk) // push the new read_through/unread to this user's other tabs/devices
	}
	w.WriteHeader(http.StatusNoContent)
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
	var body struct {
		uiSettingsPatch
		ControlToken json.RawMessage `json:"control_token"`
	}
	if err := decodeAPIRequest(r.Body, &body, true); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	sk := s.d.sessionKey(s.chatID(user))
	// Atomic create: validate + configure the session, then a SINGLE save + broadcast. A rejected
	// draft (e.g. an inaccessible cwd) creates nothing and returns the real reason; nothing external
	// ever sees a defaults placeholder, and a crash before the save leaves no half-built session.
	if len(body.ControlToken) != 0 {
		writeAPIError(w, &apiError{Code: "unsupported-control-token", Message: "Используйте ui_token или ui_read_token", status: http.StatusBadRequest})
		return
	}
	sess, createErr := s.d.createUISessionAtomic(sk, s.chatID(user), body.uiSettingsPatch)
	if createErr != nil {
		status := http.StatusBadRequest
		if ue, ok := createErr.(*uiErr); ok {
			status = ue.status
		}
		http.Error(w, createErr.Error(), status)
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
	if !s.requireSession(w, sk, body.Session) {
		return
	}
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
	if !s.requireSession(w, sk, body.Session) {
		return
	}
	if err := s.d.closeSession(sk, body.Session); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	if r.URL.Path == "/manifest.webmanifest" {
		s.serveManifest(w)
		return
	}
	if p := r.URL.Path; strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css") || strings.HasSuffix(p, ".png") {
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

func (s *uiServer) serveManifest(w http.ResponseWriter) {
	title, _ := json.Marshal(s.d.cfg.GetUITitle())
	data := bytes.ReplaceAll(manifestJSON, []byte("__KLAX_UI_TITLE_JSON__"), title)
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// serveModule serves one SPA ES module / stylesheet from the embedded ui_static dir. The
// name is constrained to a single path component (no traversal); like the shell and emoji
// font it needs no auth (the token gate is client-side). JS/CSS revalidate on reload;
// PNG filenames carry a version that must change whenever their content changes.
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
	cacheControl := "no-cache"
	if strings.HasSuffix(name, ".css") {
		ct = "text/css; charset=utf-8"
	} else if strings.HasSuffix(name, ".png") {
		ct = "image/png"
		cacheControl = "public, max-age=31536000, immutable"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", cacheControl)
	_, _ = w.Write(data)
}
