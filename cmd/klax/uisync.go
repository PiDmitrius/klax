package main

// Live UI sync: snapshot + ordered event ring (docs/CONTRACT.md §5).
//
// The detector is the only place that decides what changed. It rebuilds each session's read model
// through the stat-keyed cache, diffs it against the published value, and appends the deltas to a
// per-user ring of serialized events under one process-wide seq. Snapshots and windows are cut from
// the same published values under the detector mutex, so a client that applies the events after a
// snapshot's `at` in order holds exactly the published session state. A client that falls behind
// the ring or across a restart (another epoch) is told to resync. The server keeps no per-client state.
// Transcript checks, change collection and read reports share one interval. A changes response
// collects from its first event, then performs a fresh detection at the cut.

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
	uiSyncInterval  = 250 * time.Millisecond
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

// uiOrd orders a turn group in the feed: the transcript record of its leader, then its turn_seq. A
// queue-only turn takes the record of the transcript turn it is placed before; `last` sorts after
// every record. On the wire it is "<turn_seq>.<event>", or "<turn_seq>" for `last`.
type uiOrd struct {
	event int64
	last  bool
	seq   int64
}

func (o uiOrd) String() string {
	if o.last {
		return strconv.FormatInt(o.seq, 10)
	}
	return strconv.FormatInt(o.seq, 10) + "." + strconv.FormatInt(o.event, 10)
}

func (o uiOrd) MarshalJSON() ([]byte, error) { return json.Marshal(o.String()) }

func (o uiOrd) less(p uiOrd) bool {
	if o.last != p.last {
		return !o.last
	}
	if !o.last && o.event != p.event {
		return o.event < p.event
	}
	return o.seq < p.seq
}

// parseBound reads the "<turn_seq>.<event>" position the client sends as `to`; a range bound is
// always a transcript position.
func parseBound(v string) (uiOrd, bool) {
	s, e, ok := strings.Cut(v, ".")
	if !ok {
		return uiOrd{}, false
	}
	seq, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return uiOrd{}, false
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
	Blocks []uiBlock `json:"blocks,omitempty"`
	Rows   []uiTurn  `json:"rows,omitempty"`
}

func groupRows(klaxID string, rows []uiTurn) []uiGroup {
	out := make([]uiGroup, 0, len(rows))
	used := make(map[string]bool, len(rows))
	for i, r := range rows {
		if r.Role != "user" {
			if len(out) == 0 {
				out = append(out, uiGroup{Key: "t:0", Ord: uiOrd{event: -1}})
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
		key := fmt.Sprintf("t:%d", r.Seq)
		if used[key] {
			key = fmt.Sprintf("t:e%d", r.event)
		}
		used[key] = true
		head := new(uiTurn)
		*head = r
		head.Blocks = nil
		out = append(out, uiGroup{Key: key, Ord: ord, Head: head, Blocks: r.Blocks})
	}
	return out
}

// Deltas carry only what changed relative to the published value: an object as a JSON Merge
// Patch of its wire form (changed keys, null for a dropped key), an array as patches of the
// elements that changed by index, the elements appended, and the new length when it shrank.

// mergePatch returns the merge patch turning wire object a into b, or nil when they are equal.
func mergePatch(a, b []byte) json.RawMessage {
	if bytes.Equal(a, b) {
		return nil
	}
	var ma, mb map[string]json.RawMessage
	_ = json.Unmarshal(a, &ma)
	if json.Unmarshal(b, &mb) != nil {
		return nil
	}
	patch := make(map[string]json.RawMessage)
	for k, v := range mb {
		if !bytes.Equal(ma[k], v) {
			patch[k] = v
		}
	}
	for k := range ma {
		if _, ok := mb[k]; !ok {
			patch[k] = json.RawMessage("null")
		}
	}
	if len(patch) == 0 {
		return nil
	}
	out, _ := json.Marshal(patch)
	return out
}

// uiArrayDelta: Append starts at index Start, so applying a delta twice is harmless.
type uiArrayDelta struct {
	Set    map[string]json.RawMessage `json:"set,omitempty"`
	Start  int                        `json:"start,omitempty"`
	Append []json.RawMessage          `json:"append,omitempty"`
	Length *int                       `json:"length,omitempty"`
}

func wireList[T any](xs []T) [][]byte {
	out := make([][]byte, len(xs))
	for i, x := range xs {
		out[i], _ = json.Marshal(x)
	}
	return out
}

// arrayDelta returns the delta turning old into cur, or nil when they are equal.
func arrayDelta[T any](old, cur []T) *uiArrayDelta {
	if reflect.DeepEqual(old, cur) || (len(old) == 0 && len(cur) == 0) {
		return nil
	}
	ow, cw := wireList(old), wireList(cur)
	d := &uiArrayDelta{}
	for i := 0; i < len(ow) && i < len(cw); i++ {
		if p := mergePatch(ow[i], cw[i]); p != nil {
			if d.Set == nil {
				d.Set = make(map[string]json.RawMessage)
			}
			d.Set[strconv.Itoa(i)] = p
		}
	}
	for _, e := range cw[min(len(ow), len(cw)):] {
		d.Start = len(ow)
		d.Append = append(d.Append, e)
	}
	if len(cw) < len(ow) {
		n := len(cw)
		d.Length = &n
	}
	if d.Set == nil && d.Append == nil && d.Length == nil {
		return nil
	}
	return d
}

type uiGroupBody struct {
	Head   *uiTurn   `json:"head"`
	Blocks []uiBlock `json:"blocks,omitempty"`
	Rows   []uiTurn  `json:"rows,omitempty"`
}

// uiGroupDelta creates a group in full or patches its head, blocks and rows; key and ord address
// it either way.
type uiGroupDelta struct {
	Key    string          `json:"key"`
	Ord    uiOrd           `json:"ord"`
	Create *uiGroupBody    `json:"create,omitempty"`
	Head   json.RawMessage `json:"head,omitempty"`
	Blocks *uiArrayDelta   `json:"blocks,omitempty"`
	Rows   *uiArrayDelta   `json:"rows,omitempty"`
}

type uiRemoved struct {
	Key string `json:"key"`
	Ord uiOrd  `json:"ord"`
}

// uiEventJSON is the wire form of one ring event; exactly one payload field is set.
type uiEventJSON struct {
	Seq     uint64          `json:"seq"`
	KlaxID  string          `json:"klax_id,omitempty"`
	Group   *uiGroupDelta   `json:"group,omitempty"`
	Removed *uiRemoved      `json:"removed,omitempty"`
	Tab     json.RawMessage `json:"tab,omitempty"`  // patch of one tab
	Tabs    []string        `json:"tabs,omitempty"` // the tab order, when membership or order changed
	Notice  string          `json:"notice,omitempty"`
}

type uiPending struct {
	role int8
	ev   uiEventJSON
}

// diffGroups returns the deltas that turn old into cur: changed and new groups (group events
// first), then vanished ones.
func diffGroups(klaxID string, old, cur []uiGroup) []uiPending {
	byKey := make(map[string]*uiGroup, len(old))
	for i := range old {
		byKey[old[i].Key] = &old[i]
	}
	var out []uiPending
	live := make(map[string]bool, len(cur))
	for _, g := range cur {
		live[g.Key] = true
		d := &uiGroupDelta{Key: g.Key, Ord: g.Ord}
		o := byKey[g.Key]
		if o == nil {
			d.Create = &uiGroupBody{Head: g.Head, Blocks: g.Blocks, Rows: g.Rows}
		} else {
			if !reflect.DeepEqual(o.Head, g.Head) {
				oh, _ := json.Marshal(o.Head)
				gh, _ := json.Marshal(g.Head)
				d.Head = mergePatch(oh, gh)
			}
			d.Blocks, d.Rows = arrayDelta(o.Blocks, g.Blocks), arrayDelta(o.Rows, g.Rows)
			if d.Head == nil && d.Blocks == nil && d.Rows == nil && o.Ord == g.Ord {
				continue
			}
		}
		out = append(out, uiPending{role: roleShared, ev: uiEventJSON{KlaxID: klaxID, Group: d}})
	}
	for _, o := range old {
		if !live[o.Key] {
			out = append(out, uiPending{role: roleShared, ev: uiEventJSON{KlaxID: klaxID, Removed: &uiRemoved{Key: o.Key, Ord: o.Ord}}})
		}
	}
	return out
}

// uiTabs is one role's published tab strip: the order and each tab's wire form.
type uiTabs struct {
	order []string
	entry map[string][]byte
}

func (t *uiTabs) wire() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, c := range t.order {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(t.entry[c])
	}
	b.WriteByte(']')
	return b.Bytes()
}

// diffTabs publishes a role's strip and returns its tab patches, then its order if that changed.
func diffTabs(role int8, old *uiTabs, list []uiSessionInfo) (*uiTabs, []uiPending) {
	cur := &uiTabs{order: make([]string, len(list)), entry: make(map[string][]byte, len(list))}
	for i, t := range list {
		cur.order[i] = t.KlaxID
		cur.entry[t.KlaxID], _ = json.Marshal(t)
	}
	if old == nil {
		return cur, nil
	}
	var out []uiPending
	for _, c := range cur.order {
		before := old.entry[c]
		if before == nil {
			before = []byte("{}")
		}
		if p := mergePatch(before, cur.entry[c]); p != nil {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(p, &m)
			delete(m, "klax_id")
			tab, _ := json.Marshal(m)
			out = append(out, uiPending{role: role, ev: uiEventJSON{KlaxID: c, Tab: tab}})
		}
	}
	if !slices.Equal(old.order, cur.order) {
		out = append(out, uiPending{role: role, ev: uiEventJSON{Tabs: slices.Clone(cur.order)}})
	}
	return cur, out
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

// uiUserSync is one user's published state and event ring. pub and tabs belong to the
// detector (detMu); ring, ringBytes and floor belong to uiHub.mu.
type uiUserSync struct {
	detMu sync.Mutex
	pub   map[string]*uiPubSession
	tabs  [2]*uiTabs
	roles [2]bool // roles that requested state; a role's strip is published from its first request

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
		u = &uiUserSync{pub: make(map[string]*uiPubSession)}
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

func (h *uiHub) cursor(seq uint64) string {
	return h.epoch + "." + strconv.FormatUint(seq, 10)
}

// parseAfter reads a client cursor; ok is false for another epoch or a malformed value.
func (h *uiHub) parseAfter(v string) (uint64, bool) {
	e, s, found := strings.Cut(v, ".")
	if !found || e != h.epoch {
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
func (d *daemon) uiSync(user, sk string, role int8, cut func(u *uiUserSync, at uint64)) {
	u := d.uiHub.userSync(user)
	u.detMu.Lock()
	defer u.detMu.Unlock()
	u.roles[role] = true
	at := d.uiDetectLocked(user, sk, u)
	if cut != nil {
		cut(u, at)
	}
}

// uiDetectLocked publishes the user's changes and returns the seq the published state corresponds
// to; a notice appended later gets a higher seq.
func (d *daemon) uiDetectLocked(user, sk string, u *uiUserSync) uint64 {
	h := d.uiHub
	var evs []uiPending
	sessions := d.store.SessionsFor(sk)
	live := make(map[string]bool, len(sessions))
	rowsOf := make(map[string][]uiTurn, len(sessions))
	for _, sess := range sessions {
		live[sess.KlaxID] = true
		rows, build, ok := d.readModelBuild(sk, sess)
		rowsOf[sess.KlaxID] = rows
		if !ok {
			continue
		}
		p := u.pub[sess.KlaxID]
		if p != nil && p.build == build {
			continue
		}
		groups := groupRows(sess.KlaxID, rows)
		if p != nil {
			evs = append(evs, diffGroups(sess.KlaxID, p.groups, groups)...)
		}
		u.pub[sess.KlaxID] = &uiPubSession{build: build, groups: groups}
	}
	for klaxID := range u.pub {
		if !live[klaxID] {
			delete(u.pub, klaxID)
			d.dropReadModel(sk, klaxID)
		}
	}
	for _, role := range []int8{roleRW, roleRO} {
		if !u.roles[role] {
			continue
		}
		tabs, tabEvs := diffTabs(role, u.tabs[role], d.sessionsSnapshot(sk, sessions, rowsOf, role == roleRO))
		u.tabs[role] = tabs
		evs = append(evs, tabEvs...)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(evs) > 0 {
		h.appendLocked(user, u, evs)
	}
	return h.seq
}

// --- handlers ---

func (s *uiServer) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiFail(w, http.StatusMethodNotAllowed, "method-not-allowed", "Метод не поддерживается")
		return
	}
	user, ok := s.auth(r)
	if !ok {
		apiFail(w, http.StatusUnauthorized, "unauthorized", "Требуется авторизация")
		return
	}
	readOnly := s.readOnly(r)
	sk := s.d.sessionKey(s.chatID(user))
	if !readOnly {
		s.d.ensureSessionWithCWD(sk, s.d.sessionCWD(s.chatID(user)))
	}
	var resp struct {
		At       string          `json:"at"`
		System   systemView      `json:"system"`
		Sessions json.RawMessage `json:"sessions"`
	}
	s.d.uiSync(user, sk, roleOf(readOnly), func(u *uiUserSync, at uint64) {
		resp.At, resp.Sessions = s.d.uiHub.cursor(at), u.tabs[roleOf(readOnly)].wire()
	})
	resp.System = s.d.systemView()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleChanges is the long-poll: it answers with the ring events after `after` for the request's
// role, holding the request while there are none unless `nowait` asks for an immediate answer.
func (s *uiServer) handleChanges(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth(r)
	if !ok {
		apiFail(w, http.StatusUnauthorized, "unauthorized", "Требуется авторизация")
		return
	}
	if r.Method != http.MethodPost {
		apiFail(w, http.StatusMethodNotAllowed, "method-not-allowed", "Метод не поддерживается")
		return
	}
	var req struct {
		After  string `json:"after"`
		NoWait bool   `json:"nowait"`
	}
	if err := decodeAPIRequest(r.Body, &req, false); err != nil {
		apiFail(w, http.StatusBadRequest, "bad-request", "Некорректный запрос")
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
		apiFail(w, http.StatusTooManyRequests, "too-many-polls", "Слишком много одновременных запросов")
		return
	}
	defer h.leavePoll(poll)
	role := roleOf(s.readOnly(r))
	sk := s.d.sessionKey(s.chatID(user))
	deadline := time.NewTimer(uiPollHold)
	defer deadline.Stop()
	collect := func() ([]json.RawMessage, uint64, bool) {
		s.d.uiSync(user, sk, role, nil)
		return h.collect(user, after, role)
	}
	expired := req.NoWait
	for {
		ch := h.waitChan(user) // grab BEFORE detecting (lost-wakeup-safe)
		events, at, resync := collect()
		if resync {
			_, _ = w.Write([]byte(`{"resync":true}` + "\n"))
			return
		}
		if len(events) > 0 {
			deadline.Stop()
			batch := time.NewTimer(uiSyncInterval)
			defer batch.Stop()
			select {
			case <-batch.C:
			case <-r.Context().Done():
				return
			}
			events, at, resync = collect()
			if resync {
				_, _ = w.Write([]byte(`{"resync":true}` + "\n"))
			} else {
				writeChanges(w, h.cursor(at), events)
			}
			return
		}
		if expired {
			writeChanges(w, h.cursor(after), nil)
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			expired = true
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
	if f, ok := w.(http.Flusher); ok {
		f.Flush() // out before the poll leaves, so a restart waiting on held polls has sent it
	}
}

// handleTranscript returns a window of a session's turn groups cut from the published state: the
// newest `limit` groups, or those before the `to` ord for an older page.
func (s *uiServer) handleTranscript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiFail(w, http.StatusMethodNotAllowed, "method-not-allowed", "Метод не поддерживается")
		return
	}
	user, ok := s.auth(r)
	if !ok {
		apiFail(w, http.StatusUnauthorized, "unauthorized", "Требуется авторизация")
		return
	}
	klaxID := r.URL.Query().Get("klax_id")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var to *uiOrd
	if v := r.URL.Query().Get("to"); v != "" {
		o, ok := parseBound(v)
		if !ok {
			apiFail(w, http.StatusBadRequest, "bad-request", "Некорректная граница to")
			return
		}
		to = &o
	}
	sk := s.d.sessionKey(s.chatID(user))
	if s.d.store.Get(sk, klaxID) == nil {
		writeAPIError(w, apiFailure("session-not-found"))
		return
	}
	var resp struct {
		At     string    `json:"at"`
		From   *uiOrd    `json:"from,omitempty"` // absent when the window starts at the history start
		More   bool      `json:"more"`
		Groups []uiGroup `json:"groups"`
	}
	found := false
	s.d.uiSync(user, sk, roleOf(s.readOnly(r)), func(u *uiUserSync, at uint64) {
		p := u.pub[klaxID]
		if p == nil {
			return
		}
		found = true
		end := len(p.groups)
		if to != nil {
			end = 0
			for end < len(p.groups) && p.groups[end].Ord.less(*to) {
				end++
			}
		}
		start := max(0, end-limit)
		for start > 0 && p.groups[start].Head != nil && p.groups[start].Head.queueOnly {
			start-- // a window starts at a transcript position, which no queued turn can move below
		}
		resp.At, resp.More = s.d.uiHub.cursor(at), start > 0
		resp.Groups = slices.Clone(p.groups[start:end])
		if start > 0 {
			from := p.groups[start].Ord
			resp.From = &from
		}
	})
	if !found {
		apiFail(w, http.StatusServiceUnavailable, "history-unavailable", "История недоступна")
		return
	}
	if resp.Groups == nil {
		resp.Groups = []uiGroup{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
