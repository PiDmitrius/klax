package main

// Live UI sync: snapshot + ordered event ring (docs/CONTRACT.md §5).
//
// The detector is the only place that decides what changed. It rebuilds each session's read model
// through the stat-keyed cache, diffs it against the published value, and appends the deltas to a
// per-user ring of serialized events under one process-wide seq. Snapshots and windows are cut from
// the same published values under the detector mutex, so a client that applies the events after a
// snapshot's `at` in order holds exactly the server state. A client that falls behind the ring or
// across a restart (another epoch) is told to resync. The server keeps no per-client state.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	uiRingSoftBytes = 4 << 20 // evict while above this…
	uiRingMinEvents = 128     // …and while more than this many events remain
)

const (
	roleShared int8 = -1
	roleRW     int8 = 0
	roleRO     int8 = 1
)

func roleOf(readOnly bool) int8 {
	if readOnly {
		return roleRO
	}
	return roleRW
}

// uiOrd positions a turn group: the transcript record of its leader, then its seq. A queue-only
// turn takes the record of the transcript turn it is placed before; `last` (null on the wire)
// sorts after every record.
type uiOrd struct {
	event int64
	last  bool
	seq   int64
}

func (o uiOrd) MarshalJSON() ([]byte, error) {
	if o.last {
		return fmt.Appendf(nil, "[null,%d]", o.seq), nil
	}
	return fmt.Appendf(nil, "[%d,%d]", o.event, o.seq), nil
}

func (o uiOrd) less(p uiOrd) bool {
	if o.last != p.last {
		return !o.last
	}
	if !o.last && o.event != p.event {
		return o.event < p.event
	}
	return o.seq < p.seq
}

// parseOrd reads the "<event|null>,<seq>" form the client sends as `before`.
func parseOrd(v string) (uiOrd, bool) {
	e, s, ok := strings.Cut(v, ",")
	if !ok {
		return uiOrd{}, false
	}
	seq, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return uiOrd{}, false
	}
	if e == "null" {
		return uiOrd{last: true, seq: seq}, true
	}
	ev, err := strconv.ParseInt(e, 10, 64)
	if err != nil {
		return uiOrd{}, false
	}
	return uiOrd{event: ev, seq: seq}, true
}

// uiGroup is one turn group: a user row (head, without blocks) with its answer blocks and the
// standalone rows that follow it. Group 0 holds the standalone rows before the first user row.
type uiGroup struct {
	Key    string    `json:"key"`
	Ord    uiOrd     `json:"ord"`
	Head   *uiTurn   `json:"head"`
	Blocks []uiBlock `json:"blocks"`
	Rows   []uiTurn  `json:"rows"`
}

func groupRows(created int64, rows []uiTurn) []uiGroup {
	out := make([]uiGroup, 0, len(rows))
	used := make(map[string]bool, len(rows))
	for i, r := range rows {
		if r.Role != "user" {
			if len(out) == 0 {
				out = append(out, uiGroup{Key: fmt.Sprintf("t:%d:0", created), Ord: uiOrd{event: -1}})
			}
			out[len(out)-1].Rows = append(out[len(out)-1].Rows, r)
			continue
		}
		ord := uiOrd{event: r.event, seq: r.Seq}
		if r.queueOnly {
			ord = uiOrd{last: true, seq: r.Seq}
			for _, n := range rows[i+1:] {
				if n.Role == "user" && !n.queueOnly {
					ord = uiOrd{event: n.event, seq: r.Seq}
					break
				}
			}
		}
		key := fmt.Sprintf("t:%d:%d", created, r.Seq)
		if used[key] {
			key = fmt.Sprintf("t:%d:e%d", created, r.event)
		}
		used[key] = true
		head := new(uiTurn)
		*head = r
		head.Blocks = nil
		out = append(out, uiGroup{Key: key, Ord: ord, Head: head, Blocks: r.Blocks})
	}
	return out
}

// headEqual compares two heads apart from their context usage, which a group delta carries on
// its own so a running turn's growing usage never resends the user's text.
func headEqual(a, b *uiTurn) bool {
	if a == nil || b == nil {
		return a == b
	}
	x, y := *a, *b
	x.CtxUsed, x.CtxWindow, y.CtxUsed, y.CtxWindow = 0, 0, 0, 0
	return reflect.DeepEqual(x, y)
}

func ctxOf(h *uiTurn) [2]int {
	if h == nil {
		return [2]int{}
	}
	return [2]int{h.CtxUsed, h.CtxWindow}
}

type uiGroupDelta struct {
	Key    string    `json:"key"`
	Ord    uiOrd     `json:"ord"`
	Head   *uiTurn   `json:"head,omitempty"`
	Ctx    *[2]int   `json:"ctx,omitempty"` // [used, window] when only the head's context changed
	N      int       `json:"n"`
	From   int       `json:"from"`
	Blocks []uiBlock `json:"blocks"`
	Rows   *[]uiTurn `json:"rows,omitempty"`
}

type uiRemoved struct {
	Key string `json:"key"`
	Ord uiOrd  `json:"ord"`
}

// uiEventJSON is the wire form of one ring event; exactly one payload field is set.
type uiEventJSON struct {
	Seq      uint64          `json:"seq"`
	Session  int64           `json:"session,omitempty"`
	Group    *uiGroupDelta   `json:"group,omitempty"`
	Removed  *uiRemoved      `json:"removed,omitempty"`
	Sessions json.RawMessage `json:"sessions,omitempty"`
	Notice   string          `json:"notice,omitempty"`
}

type uiPending struct {
	role int8
	ev   uiEventJSON
}

// diffGroups returns the deltas that turn old into cur: changed and new groups (group events
// first), then vanished ones.
func diffGroups(created int64, old, cur []uiGroup) []uiPending {
	byKey := make(map[string]*uiGroup, len(old))
	for i := range old {
		byKey[old[i].Key] = &old[i]
	}
	var out []uiPending
	live := make(map[string]bool, len(cur))
	for _, g := range cur {
		live[g.Key] = true
		d := &uiGroupDelta{Key: g.Key, Ord: g.Ord, N: len(g.Blocks)}
		o := byKey[g.Key]
		if o == nil {
			d.Head, d.Blocks = g.Head, g.Blocks
			if len(g.Rows) > 0 {
				d.Rows = &g.Rows
			}
		} else {
			from := 0
			for from < len(g.Blocks) && from < len(o.Blocks) && reflect.DeepEqual(g.Blocks[from], o.Blocks[from]) {
				from++
			}
			headChanged, rowsChanged := !headEqual(o.Head, g.Head), !reflect.DeepEqual(o.Rows, g.Rows)
			ctxChanged := ctxOf(o.Head) != ctxOf(g.Head)
			if from == len(g.Blocks) && len(o.Blocks) == len(g.Blocks) && !headChanged && !ctxChanged && !rowsChanged && o.Ord == g.Ord {
				continue
			}
			d.From, d.Blocks = from, g.Blocks[from:]
			if headChanged {
				d.Head = g.Head
			} else if ctxChanged {
				c := ctxOf(g.Head)
				d.Ctx = &c
			}
			if rowsChanged {
				rows := g.Rows
				if rows == nil {
					rows = []uiTurn{}
				}
				d.Rows = &rows
			}
		}
		out = append(out, uiPending{role: roleShared, ev: uiEventJSON{Session: created, Group: d}})
	}
	for _, o := range old {
		if !live[o.Key] {
			out = append(out, uiPending{role: roleShared, ev: uiEventJSON{Session: created, Removed: &uiRemoved{Key: o.Key, Ord: o.Ord}}})
		}
	}
	return out
}

type uiRingEvent struct {
	seq  uint64
	role int8
	data json.RawMessage
}

type uiPubSession struct {
	build  uint64
	groups []uiGroup
}

// uiUserSync is one user's published state and event ring. pub and sessions belong to the
// detector (detMu); ring, ringBytes and floor belong to uiHub.mu.
type uiUserSync struct {
	detMu    sync.Mutex
	pub      map[int64]*uiPubSession
	sessions [2]json.RawMessage

	ring      []uiRingEvent
	ringBytes int
	floor     [2]uint64
}

// uiPoll is one held /api/changes request: the per-user cap counts them, and the restart-notice
// wait reads their cursors.
type uiPoll struct {
	user  string
	after uint64
}

func (h *uiHub) userSync(user string) *uiUserSync {
	h.mu.Lock()
	defer h.mu.Unlock()
	u := h.users[user]
	if u == nil {
		u = &uiUserSync{pub: make(map[int64]*uiPubSession)}
		h.users[user] = u
	}
	return u
}

// appendLocked numbers and stores events in order, evicts past the bounds, and wakes held polls.
// Caller holds h.mu.
func (h *uiHub) appendLocked(user string, u *uiUserSync, evs []uiPending) {
	for _, p := range evs {
		h.seq++
		p.ev.Seq = h.seq
		data, err := json.Marshal(p.ev)
		if err != nil {
			continue
		}
		u.ring = append(u.ring, uiRingEvent{seq: h.seq, role: p.role, data: data})
		u.ringBytes += len(data)
	}
	n := 0
	for len(u.ring)-n > uiRingMinEvents && u.ringBytes > uiRingSoftBytes {
		e := u.ring[n]
		u.ringBytes -= len(e.data)
		if e.role == roleShared || e.role == roleRW {
			u.floor[roleRW] = e.seq
		}
		if e.role == roleShared || e.role == roleRO {
			u.floor[roleRO] = e.seq
		}
		n++
	}
	if n > 0 {
		u.ring = slices.Clone(u.ring[n:])
	}
	h.wakeLocked(user)
}

func (h *uiHub) wakeLocked(user string) {
	if ch := h.notify[user]; ch != nil {
		close(ch)
		delete(h.notify, user)
	}
}

// collect returns the events after `after` visible to role, the current seq, and whether the
// client lost events (evicted past its cursor) and must resync.
func (h *uiHub) collect(user string, after uint64, role int8) (events []json.RawMessage, at uint64, resync bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	at = h.seq
	u := h.users[user]
	if u == nil {
		return nil, at, false
	}
	if after < u.floor[role] {
		return nil, at, true
	}
	for _, e := range u.ring {
		if e.seq > after && (e.role == roleShared || e.role == role) {
			events = append(events, e.data)
		}
	}
	return events, at, false
}

func (h *uiHub) at() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

func (h *uiHub) cursor(seq uint64) string {
	return strconv.FormatInt(h.epoch, 10) + "." + strconv.FormatUint(seq, 10)
}

// parseAfter reads a client cursor; ok is false for another epoch or a malformed value.
func (h *uiHub) parseAfter(v string) (uint64, bool) {
	e, s, found := strings.Cut(v, ".")
	if !found || e != strconv.FormatInt(h.epoch, 10) {
		return 0, false
	}
	seq, err := strconv.ParseUint(s, 10, 64)
	return seq, err == nil
}

// notice appends a transient notice to one user's ring.
func (h *uiHub) notice(user, text string) {
	if user == "" || text == "" {
		return
	}
	u := h.userSync(user)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.appendLocked(user, u, []uiPending{{role: roleShared, ev: uiEventJSON{Notice: text}}})
}

// noticeAll appends a notice to every user the hub serves and returns the newest seq.
func (h *uiHub) noticeAll(text string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	for user, u := range h.users {
		h.appendLocked(user, u, []uiPending{{role: roleShared, ev: uiEventJSON{Notice: text}}})
	}
	return h.seq
}

// enterPoll registers a held poll unless the user is at uiMaxInflightPerUser.
func (h *uiHub) enterPoll(user string, after uint64) (*uiPoll, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for p := range h.polls {
		if p.user == user {
			n++
		}
	}
	if n >= uiMaxInflightPerUser {
		return nil, false
	}
	p := &uiPoll{user: user, after: after}
	h.polls[p] = struct{}{}
	return p, true
}

func (h *uiHub) leavePoll(p *uiPoll) {
	h.mu.Lock()
	delete(h.polls, p)
	close(h.pollsWake)
	h.pollsWake = make(chan struct{})
	h.mu.Unlock()
}

// waitPollsPast waits, up to timeout, until no in-flight poll still has a cursor before seq.
func (h *uiHub) waitPollsPast(seq uint64, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		h.mu.Lock()
		behind := false
		for p := range h.polls {
			if p.after < seq {
				behind = true
				break
			}
		}
		wake := h.pollsWake
		h.mu.Unlock()
		if !behind {
			return
		}
		select {
		case <-wake:
		case <-timer.C:
			return
		}
	}
}

// uiSync runs the detector for a user under its mutex and then cut (if any) with the published
// state and the seq it corresponds to.
func (d *daemon) uiSync(user, sk string, cut func(u *uiUserSync, at uint64)) {
	h := d.uiHub
	u := h.userSync(user)
	u.detMu.Lock()
	defer u.detMu.Unlock()
	d.uiDetectLocked(user, sk, u)
	if cut != nil {
		cut(u, h.at())
	}
}

func (d *daemon) uiDetectLocked(user, sk string, u *uiUserSync) {
	h := d.uiHub
	var evs []uiPending
	sessions := d.store.SessionsFor(sk)
	live := make(map[int64]bool, len(sessions))
	rowsOf := make(map[int64][]uiTurn, len(sessions))
	for _, sess := range sessions {
		live[sess.Created] = true
		rows, build, ok := d.readModelBuild(sk, sess)
		rowsOf[sess.Created] = rows
		if !ok {
			continue
		}
		p := u.pub[sess.Created]
		if p != nil && p.build == build {
			continue
		}
		groups := groupRows(sess.Created, rows)
		if p != nil {
			evs = append(evs, diffGroups(sess.Created, p.groups, groups)...)
		}
		u.pub[sess.Created] = &uiPubSession{build: build, groups: groups}
	}
	for created := range u.pub {
		if !live[created] {
			delete(u.pub, created)
			d.dropReadModel(sk, created)
		}
	}
	for _, role := range []int8{roleRW, roleRO} {
		data, err := json.Marshal(d.sessionsSnapshot(sk, sessions, rowsOf, role == roleRO))
		if err != nil || bytes.Equal(data, u.sessions[role]) {
			continue
		}
		if u.sessions[role] != nil {
			evs = append(evs, uiPending{role: role, ev: uiEventJSON{Sessions: data}})
		}
		u.sessions[role] = data
	}
	if len(evs) > 0 {
		h.mu.Lock()
		h.appendLocked(user, u, evs)
		h.mu.Unlock()
	}
}

// --- handlers ---

func (s *uiServer) handleState(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	readOnly := s.readOnly(r)
	sk := s.d.sessionKey(s.chatID(user))
	if !readOnly {
		s.d.ensureSessionWithCWD(sk, s.d.sessionCWD(s.chatID(user)))
	}
	var resp struct {
		At       string          `json:"at"`
		Started  int64           `json:"started"`
		Startup  string          `json:"startup"`
		Version  string          `json:"version"`
		Sessions json.RawMessage `json:"sessions"`
	}
	s.d.uiSync(user, sk, func(u *uiUserSync, at uint64) {
		resp.At, resp.Sessions = s.d.uiHub.cursor(at), u.sessions[roleOf(readOnly)]
	})
	resp.Started, resp.Startup, resp.Version = s.d.uiHub.epoch, s.d.startupKind, version
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleChanges is the long-poll: it answers with the ring events after `after` for the request's
// role, holding the request while there are none.
func (s *uiServer) handleChanges(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		After string `json:"after"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	h := s.d.uiHub
	after, valid := h.parseAfter(req.After)
	if !valid {
		_, _ = w.Write([]byte(`{"resync":true}` + "\n"))
		return
	}
	poll, ok := h.enterPoll(user, after)
	if !ok {
		http.Error(w, "Too many concurrent polls", http.StatusTooManyRequests)
		return
	}
	defer h.leavePoll(poll)
	role := roleOf(s.readOnly(r))
	sk := s.d.sessionKey(s.chatID(user))
	deadline := time.NewTimer(uiPollHold)
	defer deadline.Stop()
	answer := func() bool {
		s.d.uiSync(user, sk, nil)
		events, at, resync := h.collect(user, after, role)
		switch {
		case resync:
			_, _ = w.Write([]byte(`{"resync":true}` + "\n"))
		case len(events) > 0:
			writeChanges(w, h.cursor(at), events)
		default:
			return false
		}
		return true
	}
	for {
		ch := h.waitChan(user) // grab BEFORE detecting (lost-wakeup-safe)
		if answer() {
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			if !answer() {
				writeChanges(w, h.cursor(after), nil)
			}
			return
		case <-r.Context().Done():
			return
		}
	}
}

func writeChanges(w http.ResponseWriter, at string, events []json.RawMessage) {
	var b bytes.Buffer
	b.WriteString(`{"at":`)
	q, _ := json.Marshal(at)
	b.Write(q)
	if len(events) > 0 {
		b.WriteString(`,"events":[`)
		for i, e := range events {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(e)
		}
		b.WriteByte(']')
	}
	b.WriteString("}\n")
	_, _ = w.Write(b.Bytes())
}

// handleTranscript returns a window of a session's turn groups cut from the published state: the
// newest `limit` groups, or those before the `before` ord for an older page.
func (s *uiServer) handleTranscript(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	created, _ := strconv.ParseInt(r.URL.Query().Get("session"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var before *uiOrd
	if v := r.URL.Query().Get("before"); v != "" {
		o, ok := parseOrd(v)
		if !ok {
			http.Error(w, "Bad before", http.StatusBadRequest)
			return
		}
		before = &o
	}
	sk := s.d.sessionKey(s.chatID(user))
	if s.d.store.Get(sk, created) == nil {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}
	var resp struct {
		At     string    `json:"at"`
		From   uiOrd     `json:"from"`
		To     *uiOrd    `json:"to"`
		More   bool      `json:"more"`
		Groups []uiGroup `json:"groups"`
	}
	found := false
	s.d.uiSync(user, sk, func(u *uiUserSync, at uint64) {
		p := u.pub[created]
		if p == nil {
			return
		}
		found = true
		end := len(p.groups)
		if before != nil {
			end = 0
			for end < len(p.groups) && p.groups[end].Ord.less(*before) {
				end++
			}
		}
		start := max(0, end-limit)
		resp.At, resp.To, resp.More = s.d.uiHub.cursor(at), before, start > 0
		resp.Groups = slices.Clone(p.groups[start:end])
		resp.From = uiOrd{event: -1} // the history start: everything below is covered
		if start > 0 {
			resp.From = p.groups[start].Ord
		}
	})
	if !found {
		http.Error(w, "История недоступна", http.StatusServiceUnavailable)
		return
	}
	if resp.Groups == nil {
		resp.Groups = []uiGroup{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
