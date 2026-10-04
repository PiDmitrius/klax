package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PiDmitrius/klax/internal/session"
)

func (o *uiOrd) UnmarshalJSON(b []byte) error {
	var v [2]*int64
	if err := json.Unmarshal(b, &v); err != nil || v[1] == nil {
		return fmt.Errorf("bad ord %s", b)
	}
	*o = uiOrd{seq: *v[1], last: v[0] == nil}
	if v[0] != nil {
		o.event = *v[0]
	}
	return nil
}

type syncWindow struct {
	At     string    `json:"at"`
	From   uiOrd     `json:"from"`
	To     *uiOrd    `json:"to"`
	More   bool      `json:"more"`
	Groups []uiGroup `json:"groups"`
}

type syncChanges struct {
	At     string        `json:"at"`
	Resync bool          `json:"resync"`
	Events []uiEventJSON `json:"events"`
}

type syncFixture struct {
	t       *testing.T
	d       *daemon
	s       *uiServer
	created int64
	dir     string
	event   int
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	d, created := newReadModelDaemon(t)
	const cwd = "/tmp/proj"
	dir := filepath.Join(home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &syncFixture{t: t, d: d, s: &uiServer{d: d, tokens: map[string]uiAccess{"tok": {User: "alice"}}}, created: created, dir: dir}
	f.attach(created, "s1")
	prev := uiPollHold
	uiPollHold = 50 * time.Millisecond
	t.Cleanup(func() { uiPollHold = prev })
	return f
}

func (f *syncFixture) attach(created int64, id string) {
	f.d.store.UpdateSession("user:alice", created, func(s *session.Session) { s.ID, s.CWD, s.Backend = id, "/tmp/proj", "claude" })
	f.write(id)
}

// write appends transcript lines (user text "u:…", assistant text otherwise) to a session file.
func (f *syncFixture) write(id string, lines ...string) {
	f.t.Helper()
	fh, err := os.OpenFile(filepath.Join(f.dir, id+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	for _, l := range lines {
		var rec string
		if text, ok := strings.CutPrefix(l, "u:"); ok {
			rec = fmt.Sprintf(`{"type":"user","message":{"content":%q},"timestamp":"2026-10-04T10:00:%02dZ"}`, text, f.event%60)
		} else {
			rec = fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q}]},"timestamp":"2026-10-04T10:00:%02dZ"}`, l, f.event%60)
		}
		f.event++
		if _, err := fh.WriteString(rec + "\n"); err != nil {
			f.t.Fatal(err)
		}
	}
	// The transcript index keys on (mtime,size); make every append observable.
	when := time.Now().Add(time.Duration(f.event) * time.Second)
	_ = os.Chtimes(filepath.Join(f.dir, id+".jsonl"), when, when)
}

func (f *syncFixture) do(method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	f.s.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		f.t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	return w
}

func (f *syncFixture) state() (string, []uiSessionInfo) {
	var st struct {
		At       string          `json:"at"`
		Sessions []uiSessionInfo `json:"sessions"`
	}
	if err := json.Unmarshal(f.do("GET", "/api/state", "").Body.Bytes(), &st); err != nil {
		f.t.Fatal(err)
	}
	return st.At, st.Sessions
}

func (f *syncFixture) window(created int64, query string) syncWindow {
	var w syncWindow
	if err := json.Unmarshal(f.do("GET", fmt.Sprintf("/api/transcript?session=%d%s", created, query), "").Body.Bytes(), &w); err != nil {
		f.t.Fatal(err)
	}
	return w
}

func (f *syncFixture) changes(after string) syncChanges {
	var c syncChanges
	body := f.do("POST", "/api/changes", fmt.Sprintf(`{"after":%q}`, after)).Body.Bytes()
	if err := json.Unmarshal(body, &c); err != nil {
		f.t.Fatal(err, string(body))
	}
	return c
}

// replica applies windows and events by the client's rules.
type replica map[string]uiGroup

func (r replica) load(w syncWindow) {
	clear(r)
	for _, g := range w.Groups {
		r[g.Key] = g
	}
}

func (r replica) apply(t *testing.T, created int64, evs []uiEventJSON) {
	t.Helper()
	for _, ev := range evs {
		if ev.Session != created {
			continue
		}
		switch {
		case ev.Removed != nil:
			delete(r, ev.Removed.Key)
		case ev.Group != nil:
			g, held := r[ev.Group.Key]
			if !held && ev.Group.Create == nil {
				t.Fatalf("delta for a group the replica lacks: %+v", ev.Group)
			}
			if !held {
				g = uiGroup{Key: ev.Group.Key}
			}
			applyDelta(&g, ev.Group)
			r[g.Key] = g
		}
	}
}

func (r replica) list() []uiGroup {
	out := slices.Collect(func(yield func(uiGroup) bool) {
		for _, g := range r {
			if !yield(g) {
				return
			}
		}
	})
	slices.SortFunc(out, func(a, b uiGroup) int {
		if a.Ord.less(b.Ord) {
			return -1
		}
		return 1
	})
	return out
}

func sameGroups(t *testing.T, got, want []uiGroup) {
	t.Helper()
	norm := func(gs []uiGroup) string {
		for i := range gs {
			if len(gs[i].Blocks) == 0 {
				gs[i].Blocks = nil
			}
			if len(gs[i].Rows) == 0 {
				gs[i].Rows = nil
			}
		}
		b, _ := json.Marshal(gs)
		return string(b)
	}
	if g, w := norm(got), norm(want); g != w {
		t.Fatalf("replayed model differs from a reload\n got %s\nwant %s", g, w)
	}
}

// A window plus the events after its `at`, applied in order, equals a fresh window at every step:
// new turns, streamed blocks, a queued turn, its cancellation.
func TestSyncReplayEqualsReload(t *testing.T) {
	f := newSyncFixture(t)
	f.write("s1", "u:first", "one")
	f.state()
	w := f.window(f.created, "&limit=50")
	r := replica{}
	r.load(w)
	at := w.At
	step := func(name string) {
		t.Helper()
		c := f.changes(at)
		if c.Resync {
			t.Fatalf("%s: unexpected resync", name)
		}
		r.apply(t, f.created, c.Events)
		at = c.At
		sameGroups(t, r.list(), f.window(f.created, "&limit=50").Groups)
	}
	f.write("s1", "u:second")
	step("new turn")
	for i := range 3 {
		f.write("s1", fmt.Sprint("block ", i))
		step(fmt.Sprint("block ", i))
	}
	sr := f.d.getRunner("user:alice", f.created)
	seq, _, _, _, err := sr.store.Enqueue("ui:alice", "", "nq", "queued", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.d.uiPoke("alice")
	step("queued turn")
	if err := sr.store.MarkErr(seq, turnErrCancelled, 0); err != nil {
		t.Fatal(err)
	}
	step("cancelled turn")
}

func TestSyncIdlePollReturnsSameAt(t *testing.T) {
	f := newSyncFixture(t)
	at, _ := f.state()
	body := f.do("POST", "/api/changes", fmt.Sprintf(`{"after":%q}`, at)).Body.String()
	if want := fmt.Sprintf(`{"at":%q}`, at) + "\n"; body != want {
		t.Fatalf("idle poll = %q, want %q", body, want)
	}
}

func TestSyncRenameSendsOnlyTheName(t *testing.T) {
	f := newSyncFixture(t)
	at, _ := f.state()
	f.d.renameSession("user:alice", f.created, "renamed")
	c := f.changes(at)
	if want := fmt.Sprintf(`{"created":%d,"name":"renamed"}`, f.created); len(c.Events) != 1 || string(c.Events[0].Tab) != want {
		t.Fatalf("rename events = %+v, want %s", c.Events, want)
	}
}

// The strip replayed from tab patches and orders equals a fresh snapshot.
func TestSyncTabsReplayEqualsState(t *testing.T) {
	f := newSyncFixture(t)
	at, list := f.state()
	tabs := map[int64][]byte{}
	var order []int64
	for _, s := range list {
		tabs[s.Created], _ = json.Marshal(s)
		order = append(order, s.Created)
	}
	added := f.d.store.New("user:alice", "two", "/tmp/proj", session.ScopeDefaults{})
	f.d.renameSession("user:alice", f.created, "renamed")
	f.d.store.UpdateSession("user:alice", f.created, func(s *session.Session) { s.Groups = []string{"g"} })
	f.d.uiPoke("alice")
	c := f.changes(at)
	f.d.store.UpdateSession("user:alice", f.created, func(s *session.Session) { s.Groups = nil })
	if err := f.d.closeSession("user:alice", added.Created); err != nil {
		t.Fatal(err)
	}
	c2 := f.changes(c.At)
	for _, ev := range append(c.Events, c2.Events...) {
		switch {
		case ev.Tab != nil:
			var id struct{ Created int64 }
			_ = json.Unmarshal(ev.Tab, &id)
			tabs[id.Created] = applyMerge(tabs[id.Created], ev.Tab)
		case ev.Tabs != nil:
			order = ev.Tabs
		}
	}
	_, fresh := f.state()
	var got []uiSessionInfo
	for _, c := range order {
		var s uiSessionInfo
		_ = json.Unmarshal(tabs[c], &s)
		got = append(got, s)
	}
	if !wireEqual(got, fresh) {
		t.Fatalf("replayed strip differs from a snapshot\n got %+v\nwant %+v", got, fresh)
	}
}

// A session that appears with history, or leaves, never streams its groups as events; a session
// attached to another transcript streams the difference like any other change.
func TestSyncSessionLifecycleEvents(t *testing.T) {
	f := newSyncFixture(t)
	at, _ := f.state()
	f.write("s2", "u:old", "a", "u:older", "b")
	added := f.d.store.New("user:alice", "two", "/tmp/proj", session.ScopeDefaults{})
	f.attach(added.Created, "s2")
	c := f.changes(at)
	for _, ev := range c.Events {
		if ev.Group != nil || ev.Removed != nil {
			t.Fatalf("new session streamed its history: %+v", ev)
		}
	}
	w := f.window(added.Created, "&limit=50")
	r := replica{}
	r.load(w)
	f.write("s3", "u:other")
	f.attach(added.Created, "s3")
	c = f.changes(w.At)
	r.apply(t, added.Created, c.Events)
	sameGroups(t, r.list(), f.window(added.Created, "&limit=50").Groups)
	at = c.At
	if err := f.d.closeSession("user:alice", added.Created); err != nil {
		t.Fatal(err)
	}
	c = f.changes(at)
	for _, ev := range c.Events {
		if ev.Group != nil || ev.Removed != nil {
			t.Fatalf("closed session streamed group events: %+v", ev)
		}
	}
	if n := len(c.Events); n == 0 || c.Events[n-1].Tabs == nil {
		t.Fatalf("close events = %+v, want the new order last", c.Events)
	}
}

// A model change with another window updates the strip only; finished turns keep the window
// their done record holds.
func TestSyncSessionWindowLeavesTurnsAlone(t *testing.T) {
	f := newSyncFixture(t)
	f.write("s1", "u:hello", "hi")
	at, _ := f.state()
	f.d.store.UpdateSession("user:alice", f.created, func(s *session.Session) { s.ContextWindow = 1_000_000 })
	f.d.uiPoke("alice")
	c := f.changes(at)
	if want := fmt.Sprintf(`{"created":%d,"ctx_window":1000000}`, f.created); len(c.Events) != 1 || string(c.Events[0].Tab) != want {
		t.Fatalf("window change events = %+v, want only %s", c.Events, want)
	}
}

func TestSyncWindowPaging(t *testing.T) {
	f := newSyncFixture(t)
	for i := range 7 {
		f.write("s1", fmt.Sprint("u:q", i), fmt.Sprint("a", i))
	}
	all := f.window(f.created, "&limit=100")
	if len(all.Groups) != 7 || all.More || all.From != (uiOrd{event: -1}) {
		t.Fatalf("full window = %d groups, more=%v, from=%+v; want 7, false, the history start", len(all.Groups), all.More, all.From)
	}
	var got []uiGroup
	w := f.window(f.created, "&limit=3")
	got = append(got, w.Groups...)
	for w.More {
		before := w.From
		ord := fmt.Sprintf("%d,%d", before.event, before.seq)
		w = f.window(f.created, "&limit=3&before="+ord)
		if w.To == nil || *w.To != before {
			t.Fatalf("page to = %+v, want %+v", w.To, before)
		}
		got = append(slices.Clone(w.Groups), got...)
	}
	sameGroups(t, got, all.Groups)
}

func TestSyncResyncAcrossProcesses(t *testing.T) {
	f := newSyncFixture(t)
	for _, after := range []string{"", fmt.Sprintf("%d.1", f.d.uiHub.epoch+1)} {
		if c := f.changes(after); !c.Resync {
			t.Fatalf("after %q: %+v, want resync", after, c)
		}
	}
}

func TestSyncNoticeReleasesHeldPoll(t *testing.T) {
	f := newSyncFixture(t)
	at, _ := f.state()
	uiPollHold = 5 * time.Second
	done := make(chan syncChanges, 1)
	go func() { done <- f.changes(at) }()
	time.Sleep(20 * time.Millisecond)
	f.d.uiNotice("alice", "hello")
	select {
	case c := <-done:
		if len(c.Events) != 1 || c.Events[0].Notice != "hello" {
			t.Fatalf("events = %+v", c.Events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a notice did not release the held poll")
	}
}

// A window never starts at a queued turn: once that turn reaches the transcript its position would
// fall below the range and the client would drop it.
func TestSyncWindowStartsAtTranscriptPosition(t *testing.T) {
	f := newSyncFixture(t)
	f.write("s1", "u:first", "one")
	sr := f.d.getRunner("user:alice", f.created)
	for i := range 4 {
		if _, _, _, _, err := sr.store.Enqueue("ui:alice", "", fmt.Sprint("n", i), fmt.Sprint("queued ", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	f.state()
	w := f.window(f.created, "&limit=2")
	if w.From.last || len(w.Groups) < 3 {
		t.Fatalf("window from=%+v with %d groups, want it to start at the transcript turn", w.From, len(w.Groups))
	}
}
