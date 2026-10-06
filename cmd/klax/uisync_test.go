package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/ids"
	"github.com/PiDmitrius/klax/internal/session"
)

func (o *uiOrd) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("bad ord %s", b)
	}
	if !strings.Contains(v, ".") {
		seq, err := strconv.ParseInt(v, 10, 64)
		*o = uiOrd{seq: seq, last: true}
		return err
	}
	p, ok := parseBound(v)
	if !ok {
		return fmt.Errorf("bad ord %s", b)
	}
	*o = p
	return nil
}

type syncWindow struct {
	At     string    `json:"at"`
	From   *uiOrd    `json:"from"`
	More   bool      `json:"more"`
	Groups []uiGroup `json:"groups"`
}

type syncChanges struct {
	At     string        `json:"at"`
	Resync bool          `json:"resync"`
	Events []uiEventJSON `json:"events"`
}

type syncFixture struct {
	t      *testing.T
	d      *daemon
	s      *uiServer
	klaxID string
	dir    string
	event  int
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	d, klaxID := newReadModelDaemon(t)
	d.cfg = &config.Config{}
	store, err := session.LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	store.Chats, store.Scope = d.store.Chats, d.store.Scope
	d.store = store
	const cwd = "/tmp/proj"
	dir := filepath.Join(home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &syncFixture{t: t, d: d, s: &uiServer{d: d, tokens: map[string]uiAccess{"tok": {User: "alice"}}}, klaxID: klaxID, dir: dir}
	f.attach(klaxID, "s1")
	prev := uiPollHold
	uiPollHold = 50 * time.Millisecond
	t.Cleanup(func() { uiPollHold = prev })
	return f
}

func (f *syncFixture) attach(klaxID string, id string) {
	f.d.store.UpdateSession("user:alice", klaxID, func(s *session.Session) { s.BackendID, s.CWD, s.Backend = id, "/tmp/proj", "claude" })
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

func TestStateIncludesSystemSnapshot(t *testing.T) {
	f := newSyncFixture(t)
	f.d.startupKind = "installed"
	f.d.system = newSystemState(time.Now().Add(-time.Minute))
	f.d.system.lastVersion = "9.9.9"
	decode := func(raw []byte) map[string]json.RawMessage {
		t.Helper()
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}
	state := decode(f.do("GET", "/api/state", "").Body.Bytes())
	if len(state) != 3 || state["at"] == nil || state["sessions"] == nil || state["system"] == nil {
		t.Fatalf("state fields = %v", state)
	}
	var view systemView
	if err := json.Unmarshal(state["system"], &view); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if view.Version != version || view.Startup != "installed" || view.Home != home || view.Update.Installed != "9.9.9" {
		t.Fatalf("system metadata = %+v", view)
	}
	inState := decode(state["system"])
	standalone := decode(f.do("GET", "/api/system", "").Body.Bytes())
	if update := decode(inState["update"]); update["current"] != nil {
		t.Fatal("system update duplicates the running version")
	}
	for _, field := range []string{"uptime_sec", "cpu_time_sec", "rss_bytes", "rss_peak_bytes"} {
		if inState[field] == nil || standalone[field] == nil {
			t.Fatalf("missing sampled field %q", field)
		}
		delete(inState, field)
		delete(standalone, field)
	}
	if !reflect.DeepEqual(inState, standalone) {
		t.Fatalf("state system = %v, standalone system = %v", inState, standalone)
	}
	f.d.system.startedAt = time.Now().Add(-24 * time.Hour)
	next := decode(f.do("GET", "/api/state", "").Body.Bytes())
	if err := json.Unmarshal(next["system"], &view); err != nil {
		t.Fatal(err)
	}
	if string(next["at"]) != string(state["at"]) || view.UptimeSec < 86400 {
		t.Fatalf("system sample changed session cursor: at = %s, uptime = %d", next["at"], view.UptimeSec)
	}
}

func (f *syncFixture) window(klaxID string, query string) syncWindow {
	var w syncWindow
	if err := json.Unmarshal(f.do("GET", fmt.Sprintf("/api/transcript?klax_id=%s%s", klaxID, query), "").Body.Bytes(), &w); err != nil {
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

func (r replica) apply(t *testing.T, klaxID string, evs []uiEventJSON) {
	t.Helper()
	for _, ev := range evs {
		if ev.KlaxID != klaxID {
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
	w := f.window(f.klaxID, "&limit=50")
	r := replica{}
	r.load(w)
	at := w.At
	step := func(name string) {
		t.Helper()
		c := f.changes(at)
		if c.Resync {
			t.Fatalf("%s: unexpected resync", name)
		}
		r.apply(t, f.klaxID, c.Events)
		at = c.At
		sameGroups(t, r.list(), f.window(f.klaxID, "&limit=50").Groups)
	}
	f.write("s1", "u:second")
	step("new turn")
	for i := range 3 {
		f.write("s1", fmt.Sprint("block ", i))
		step(fmt.Sprint("block ", i))
	}
	sr := f.d.getRunner("user:alice", f.klaxID)
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

func TestSyncChangesBatchHasFixedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t)
		uiPollHold = 5 * time.Second
		at, _ := f.state()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := httptest.NewRequest("POST", "/api/changes", strings.NewReader(fmt.Sprintf(`{"after":%q}`, at))).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		done := make(chan time.Time, 1)
		go func() { f.s.routes().ServeHTTP(w, r); done <- time.Now() }()
		synctest.Wait()
		f.d.uiPoke("alice")
		synctest.Wait()
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("an empty wake released the poll")
		default:
		}
		start := time.Now()
		f.d.uiNotice("alice", "one")
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		f.d.uiNotice("alice", "two")
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		f.d.uiNotice("alice", "three")
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("poll returned before the collection window ended")
		default:
		}
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		var finished time.Time
		select {
		case finished = <-done:
		default:
			t.Fatal("later events extended the collection window")
		}
		var c syncChanges
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		if len(c.Events) != 3 {
			t.Fatalf("events = %+v, want all three notices", c.Events)
		}
		for i, notice := range []string{"one", "two", "three"} {
			if c.Events[i].Notice != notice || (i > 0 && c.Events[i].Seq <= c.Events[i-1].Seq) {
				t.Fatalf("events out of order: %+v", c.Events)
			}
		}
		if elapsed := finished.Sub(start); elapsed != 250*time.Millisecond {
			t.Fatalf("collection window = %s, want 250ms", elapsed)
		}
		f.d.uiNotice("alice", "next")
		if next := f.changes(c.At); len(next.Events) != 1 || next.Events[0].Notice != "next" {
			t.Fatalf("next response = %+v", next)
		}
	})
}

func TestSyncChangesBatchDetectsLateTranscriptUsage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t)
		uiPollHold = 5 * time.Second
		f.write("s1", "u:first")
		at, _ := f.state()
		replayed := make(replica)
		replayed.load(f.window(f.klaxID, ""))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := httptest.NewRequest("POST", "/api/changes", strings.NewReader(fmt.Sprintf(`{"after":%q}`, at))).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { f.s.routes().ServeHTTP(w, r); close(done) }()
		synctest.Wait()
		f.write("s1", "answer")
		f.d.uiPoke("alice")
		synctest.Wait()
		time.Sleep(200 * time.Millisecond)
		fh, err := os.OpenFile(filepath.Join(f.dir, "s1.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fh.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"tail"}],"usage":{"input_tokens":900}}}` + "\n")
		closeErr := fh.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("append usage: %v, close: %v", err, closeErr)
		}
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("poll did not finish its collection window")
		}
		var c syncChanges
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		replayed.apply(t, f.klaxID, c.Events)
		groups := replayed.list()
		if len(groups) != 1 || groups[0].Head.CtxUsed != 900 || len(groups[0].Blocks) != 2 {
			t.Fatalf("late transcript update missing from response: %+v", groups)
		}
		if latest, _ := f.state(); latest != c.At {
			t.Fatalf("response cursor %s omitted changes through %s", c.At, latest)
		}
	})
}

func TestSyncChangesBatchCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t)
		at, _ := f.state()
		f.d.uiNotice("alice", "hello")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := httptest.NewRequest("POST", "/api/changes", strings.NewReader(fmt.Sprintf(`{"after":%q}`, at))).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { f.s.routes().ServeHTTP(w, r); close(done) }()
		synctest.Wait()
		time.Sleep(50 * time.Millisecond)
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("cancelled request kept collecting events")
		}
		if w.Body.Len() != 0 || len(f.d.uiHub.polls) != 0 {
			t.Fatalf("cancelled request wrote %q or retained a poll", w.Body.String())
		}
	})
}

func TestSyncRenameSendsOnlyTheName(t *testing.T) {
	f := newSyncFixture(t)
	at, _ := f.state()
	f.d.renameSession("user:alice", f.klaxID, "renamed")
	c := f.changes(at)
	if want := `{"name":"renamed"}`; len(c.Events) != 1 || c.Events[0].KlaxID != f.klaxID || string(c.Events[0].Tab) != want {
		t.Fatalf("rename events = %+v, want %s", c.Events, want)
	}
}

// The strip replayed from tab patches and orders equals a fresh snapshot.
func TestSyncTabsReplayEqualsState(t *testing.T) {
	f := newSyncFixture(t)
	at, list := f.state()
	tabs := map[string][]byte{}
	var order []string
	for _, s := range list {
		tabs[s.KlaxID], _ = json.Marshal(s)
		order = append(order, s.KlaxID)
	}
	added := f.d.store.New("user:alice", "two", "/tmp/proj", session.ScopeDefaults{})
	f.d.renameSession("user:alice", f.klaxID, "renamed")
	f.d.store.UpdateSession("user:alice", f.klaxID, func(s *session.Session) { s.Groups = []string{"g"} })
	f.d.uiPoke("alice")
	c := f.changes(at)
	f.d.store.UpdateSession("user:alice", f.klaxID, func(s *session.Session) { s.Groups = nil })
	if _, err := f.d.closeSession("user:alice", added.KlaxID); err != nil {
		t.Fatal(err)
	}
	c2 := f.changes(c.At)
	for _, ev := range append(c.Events, c2.Events...) {
		switch {
		case ev.Tab != nil:
			var patch map[string]json.RawMessage
			if err := json.Unmarshal(ev.Tab, &patch); err != nil {
				t.Fatal(err)
			}
			if ev.KlaxID == "" || patch["klax_id"] != nil {
				t.Fatalf("tab event address = %+v", ev)
			}
			before := tabs[ev.KlaxID]
			if before == nil {
				before = []byte(fmt.Sprintf(`{"klax_id":%q}`, ev.KlaxID))
			}
			tabs[ev.KlaxID] = applyMerge(before, ev.Tab)
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
	f.attach(added.KlaxID, "s2")
	c := f.changes(at)
	for _, ev := range c.Events {
		if ev.Group != nil || ev.Removed != nil {
			t.Fatalf("new session streamed its history: %+v", ev)
		}
	}
	w := f.window(added.KlaxID, "&limit=50")
	r := replica{}
	r.load(w)
	f.write("s3", "u:other")
	f.attach(added.KlaxID, "s3")
	c = f.changes(w.At)
	r.apply(t, added.KlaxID, c.Events)
	sameGroups(t, r.list(), f.window(added.KlaxID, "&limit=50").Groups)
	at = c.At
	if _, err := f.d.closeSession("user:alice", added.KlaxID); err != nil {
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
	f.d.store.UpdateSession("user:alice", f.klaxID, func(s *session.Session) { s.ContextWindow = 1_000_000 })
	f.d.uiPoke("alice")
	c := f.changes(at)
	if want := `{"ctx_window":1000000}`; len(c.Events) != 1 || c.Events[0].KlaxID != f.klaxID || string(c.Events[0].Tab) != want {
		t.Fatalf("window change events = %+v, want only %s", c.Events, want)
	}
}

func TestSyncWindowPaging(t *testing.T) {
	f := newSyncFixture(t)
	for i := range 7 {
		f.write("s1", fmt.Sprint("u:q", i), fmt.Sprint("a", i))
	}
	all := f.window(f.klaxID, "&limit=100")
	if len(all.Groups) != 7 || all.More || all.From != nil {
		t.Fatalf("full window = %d groups, more=%v, from=%+v; want 7, false, the history start", len(all.Groups), all.More, all.From)
	}
	var got []uiGroup
	w := f.window(f.klaxID, "&limit=3")
	got = append(got, w.Groups...)
	for w.More {
		w = f.window(f.klaxID, "&limit=3&to="+w.From.String())
		got = append(slices.Clone(w.Groups), got...)
	}
	sameGroups(t, got, all.Groups)
}

func TestSyncResyncAcrossProcesses(t *testing.T) {
	f := newSyncFixture(t)
	for _, after := range []string{"", ids.New() + ".1"} {
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
	sr := f.d.getRunner("user:alice", f.klaxID)
	for i := range 4 {
		if _, _, _, _, err := sr.store.Enqueue("ui:alice", "", fmt.Sprint("n", i), fmt.Sprint("queued ", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	f.state()
	w := f.window(f.klaxID, "&limit=2")
	if (w.From != nil && w.From.last) || len(w.Groups) < 3 {
		t.Fatalf("window from=%+v with %d groups, want it to start at the transcript turn", w.From, len(w.Groups))
	}
}

// A role's strip is published from its first request, so a user without read-only clients adds no
// read-only tab events to the ring.
func TestSyncReadOnlyStripPublishedOnDemand(t *testing.T) {
	f := newSyncFixture(t)
	at, _ := f.state()
	f.d.renameSession("user:alice", f.klaxID, "renamed")
	f.changes(at)
	u := f.d.uiHub.userSync("alice")
	if u.tabs[roleRO] != nil {
		t.Fatal("read-only strip published without a read-only request")
	}
	after, _ := f.d.uiHub.parseAfter(at)
	if ev, _, _ := f.d.uiHub.collect("alice", after, roleRO); len(ev) != 0 {
		t.Fatalf("read-only events without a read-only client: %d", len(ev))
	}
}
