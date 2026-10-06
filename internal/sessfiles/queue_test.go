package sessfiles

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/session"
)

// An aborted queued turn (enq -> err:aborted, never run) must NOT replay.
func TestAbortedTurnDoesNotReplay(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", "s9")
	a, _, _, _, _ := s.Enqueue("tg:1", "", "a", "A", nil)
	b, _, _, _, _ := s.Enqueue("tg:1", "", "b", "B", nil)
	if err := s.MarkErr(a, "aborted", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkErr(b, "aborted", 0); err != nil {
		t.Fatal(err)
	}
	reenq, recovered, err := Open("user:alice", "s9").Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(reenq) != 0 || len(recovered) != 0 {
		t.Fatalf("aborted turns must not replay: reenq=%d recovered=%d", len(reenq), len(recovered))
	}
}

// After Remove, a late Mark*/Enqueue returns ErrRemoved and does NOT recreate the dir.
func TestRemovedStoreNotResurrected(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", "s11")
	seq, _, _, _, _ := s.Enqueue("tg:1", "", "n", "hi", nil)
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("dir should be gone after Remove")
	}
	if err := s.MarkDone(seq, 0); !errors.Is(err, ErrRemoved) {
		t.Fatalf("MarkDone after Remove = %v, want ErrRemoved", err)
	}
	if _, _, _, _, err := s.Enqueue("tg:1", "", "n2", "again", nil); !errors.Is(err, ErrRemoved) {
		t.Fatalf("Enqueue after Remove = %v, want ErrRemoved", err)
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("a late Mark/Enqueue must NOT recreate the removed dir")
	}
}

func nr(name, data string) NamedReader { return NamedReader{Name: name, R: strings.NewReader(data)} }

type failedAttachment struct{}

func (failedAttachment) Read([]byte) (int, error) { return 0, errors.New("attachment read failed") }

func TestFailedEnqueueReservesSequenceAcrossRestart(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:test", "s1")
	failed, _, _, _, err := s.Enqueue("ui:test", "", "first", "first", []NamedReader{
		nr("report.txt", "OLD"), {Name: "second.txt", R: failedAttachment{}},
	})
	if err == nil {
		t.Fatal("accepted unreadable attachment")
	}
	s = Open("user:test", "s1")
	if turns, err := s.InboundLog(); err != nil || len(turns) != 0 {
		t.Fatalf("failed acceptance became a turn: %+v, %v", turns, err)
	}
	seq, _, files, _, err := s.Enqueue("ui:test", "", "second", "second", []NamedReader{nr("report.txt", "NEW")})
	if err != nil || seq <= failed {
		t.Fatalf("sequence reused: failed=%d accepted=%d error=%v", failed, seq, err)
	}
	data, err := os.ReadFile(s.Path(files[0]))
	if err != nil || string(data) != "NEW" {
		t.Fatalf("attachment = %q, error = %v", data, err)
	}
}

func TestKeyMigrationPreservesDurableQueue(t *testing.T) {
	for _, oldKey := range []string{"tg:1", "_migrated"} {
		t.Run(oldKey, func(t *testing.T) {
			t.Setenv("KLAX_DATA_DIR", t.TempDir())
			store, err := session.LoadStore()
			if err != nil {
				t.Fatal(err)
			}
			sess := store.New(oldKey, "pending", "/tmp", session.ScopeDefaults{})
			s := Open(oldKey, sess.KlaxID)
			seq, _, files, _, err := s.Enqueue("tg:1", "1", "pending", "message", []NamedReader{nr("report.txt", "DATA")})
			if err != nil {
				t.Fatal(err)
			}
			var moved bool
			if oldKey == "_migrated" {
				moved, err = store.MigrateTo("user:test")
			} else {
				moved, err = store.MergeKeys("user:test", []string{oldKey})
			}
			if err != nil || !moved {
				t.Fatalf("migration = %v, %v", moved, err)
			}
			store, err = session.LoadStore()
			if err != nil || store.Get("user:test", sess.KlaxID) == nil || store.Get(oldKey, sess.KlaxID) != nil {
				t.Fatalf("metadata migration failed: %v", err)
			}
			s = Open("user:test", sess.KlaxID)
			pending, recovered, err := s.Replay()
			if err != nil || len(pending) != 1 || len(recovered) != 0 || pending[0].Text != "message" {
				t.Fatalf("replay = %+v, %+v, %v", pending, recovered, err)
			}
			data, err := os.ReadFile(s.Path(files[0]))
			if err != nil || string(data) != "DATA" {
				t.Fatalf("attachment = %q, %v", data, err)
			}
			got, _, _, duplicate, err := s.Enqueue("tg:1", "", "pending", "retry", nil)
			if err != nil || !duplicate || got != seq {
				t.Fatalf("nonce was lost: %d, %v, %v", got, duplicate, err)
			}
			got, _, _, _, err = s.Enqueue("tg:1", "", "next", "next", nil)
			if err != nil || got <= seq {
				t.Fatalf("sequence reset: %d, %v", got, err)
			}
		})
	}
}

// Enqueue allocates an increasing turn_seq without creating new prompt markers,
// persists files + an enq record, and the seq survives a "restart" (fresh Store
// reading the same log) — continuing from the log, not from 1.
func TestEnqueueAllocatesAndSurvivesRestart(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", "s100")
	seq1, m1, f1, _, err := s.Enqueue("ui:alice", "", "n1", "hi", []NamedReader{nr("a.png", "AA")})
	if err != nil {
		t.Fatal(err)
	}
	seq2, m2, _, _, _ := s.Enqueue("ui:alice", "", "n2", "yo", nil) // text-only turn is fine
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seqs = %d,%d want 1,2", seq1, seq2)
	}
	if m1 != "" || m2 != "" {
		t.Fatalf("new turns must be markerless: %q %q", m1, m2)
	}
	if len(f1) != 1 || f1[0] != "000001-01-a.png" {
		t.Fatalf("stored = %v want [000001-01-a.png]", f1)
	}
	if b, _ := os.ReadFile(s.Path(f1[0])); string(b) != "AA" {
		t.Fatalf("file bytes = %q want AA", b)
	}
	// Restart: a fresh Store reads the durable log and continues the sequence.
	s2 := Open("user:alice", "s100")
	log, err := s2.InboundLog()
	if err != nil || len(log) != 2 {
		t.Fatalf("InboundLog after restart = %d turns (err %v) want 2", len(log), err)
	}
	if log[0].Text != "hi" || log[0].Marker != m1 || len(log[0].Files) != 1 || log[0].ChatID != "ui:alice" {
		t.Fatalf("turn1 reload mismatch: %+v", log[0])
	}
	seq3, _, _, _, _ := s2.Enqueue("ui:alice", "", "n3", "again", nil)
	if seq3 != 3 {
		t.Fatalf("seq after restart = %d want 3", seq3)
	}
}

func TestEnqueueDedupeByNonce(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", "s101")
	seq1, marker1, files1, dup1, err := s.Enqueue("ui:alice", "", "nonce-1", "first", []NamedReader{nr("a.png", "AA")})
	if err != nil {
		t.Fatal(err)
	}
	seq2, marker2, files2, dup2, err := s.Enqueue("ui:alice", "", "nonce-1", "retry", []NamedReader{nr("b.png", "BB")})
	if err != nil {
		t.Fatal(err)
	}
	if dup1 || !dup2 {
		t.Fatalf("duplicate flags = first %v second %v, want false/true", dup1, dup2)
	}
	if seq2 != seq1 || marker2 != marker1 || len(files2) != len(files1) || files2[0] != files1[0] {
		t.Fatalf("duplicate nonce returned seq/marker/files = %d/%q/%v, want %d/%q/%v", seq2, marker2, files2, seq1, marker1, files1)
	}
	log, err := s.InboundLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Text != "first" {
		t.Fatalf("duplicate nonce must not append a second turn: %+v", log)
	}
	if _, err := os.Stat(s.Path("000002-01-b.png")); !os.IsNotExist(err) {
		t.Fatalf("duplicate nonce must not write retry attachment, stat err=%v", err)
	}
}

// Replay classifies non-terminal turns: enq-only → reenqueue; run-without-terminal
// → recover; done → skipped. Survives a fresh Store.
func TestReplayClassifies(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", "s7")
	sA, _, _, _, _ := s.Enqueue("tg:1", "", "a", "A", nil)
	s.MarkRun(sA)
	s.MarkDone(sA, 0)                    // A: complete → skipped
	s.Enqueue("tg:1", "", "b", "B", nil) // B: enq only → reenqueue
	sC, _, _, _, _ := s.Enqueue("tg:1", "", "c", "C", nil)
	s.MarkRun(sC) // C: run, no terminal → recover

	reenq, recover, err := Open("user:alice", "s7").Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(reenq) != 1 || reenq[0].Text != "B" {
		t.Fatalf("reenqueue = %v want [B]", reenq)
	}
	if len(recover) != 1 || recover[0].Text != "C" {
		t.Fatalf("recover = %v want [C]", recover)
	}
}

// A torn trailing line (crash mid-append) is skipped; valid turns survive.
func TestTornTrailingLineSkipped(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", "s1")
	s.Enqueue("tg:1", "", "n", "good", nil)
	// Simulate a crash mid-append: a partial JSON line with no newline.
	f, _ := os.OpenFile(s.queuePath(), os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(`{"ev":"enq","seq":2,"text":"tor`)
	f.Close()

	log, err := Open("user:alice", "s1").InboundLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Text != "good" {
		t.Fatalf("InboundLog = %+v, want one clean 'good' turn", log)
	}
}

func TestAppendAfterTornTailStartsCleanRecord(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", "s2")
	seq, _, _, _, _ := s.Enqueue("ui:alice", "", "n", "good", nil)
	f, _ := os.OpenFile(s.queuePath(), os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(`{"ev":"bind","seq":1`)
	_ = f.Close()
	if err := s.MarkDone(seq, 0); err != nil {
		t.Fatal(err)
	}
	turns, err := Open("user:alice", "s2").InboundLog()
	if err != nil || len(turns) != 1 || turns[0].Last != "done" {
		t.Fatalf("valid append after torn tail was lost: %+v, %v", turns, err)
	}
}

func TestRunMetadataAndBindingSurviveRestart(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", "s5")
	seq, _, _, _, _ := s.Enqueue("ui:alice", "", "n", "hello", nil)
	if err := s.MarkRunMeta(seq, "claude", "S", "prompt", 7); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(seq, "claude", "S", 9, "record"); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(seq, "claude", "S", 9, "record"); err != nil {
		t.Fatalf("idempotent bind: %v", err)
	}
	turns, err := Open("user:alice", "s5").InboundLog()
	if err != nil || len(turns) != 1 {
		t.Fatalf("reload: %+v %v", turns, err)
	}
	got := turns[0]
	if got.Backend != "claude" || got.BackendID != "S" || got.PromptDigest != "prompt" || got.FromEvent != 7 || !got.Bound || got.Event != 9 || got.RecordDigest != "record" {
		t.Fatalf("folded turn: %+v", got)
	}
	if err := s.Bind(seq, "claude", "S", 10, "other"); !errors.Is(err, ErrBindConflict) {
		t.Fatalf("conflicting bind = %v", err)
	}
}

func TestHookFailuresFoldWithoutDuplicatingTerminalState(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", "s12")

	startSeq, _, _, _, _ := s.Enqueue("ui:alice", "", "start", "blocked", nil)
	if err := s.MarkRun(startSeq); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHookError(startSeq, "audit.turn.start", "audit-start-failed"); err != nil {
		t.Fatal(err)
	}

	finishSeq, _, _, _, _ := s.Enqueue("ui:alice", "", "finish", "completed", nil)
	if err := s.MarkRun(finishSeq); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(finishSeq, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHookError(finishSeq, "audit.turn.finish", "audit-finish-failed"); err != nil {
		t.Fatal(err)
	}

	turns, err := Open("user:alice", "s12").InboundLog()
	if err != nil {
		t.Fatal(err)
	}
	if turns[0].Last != "err" || turns[0].Reason != "audit-start-failed" || len(turns[0].HookFailures) != 1 {
		t.Fatalf("start hook fold = %+v", turns[0])
	}
	if turns[1].Last != "done" || turns[1].Reason != "" || len(turns[1].HookFailures) != 1 {
		t.Fatalf("finish hook fold = %+v", turns[1])
	}
	reenqueue, recover, err := Open("user:alice", "s12").Replay()
	if err != nil || len(reenqueue) != 0 || len(recover) != 0 {
		t.Fatalf("hook failures replayed: enq=%v recover=%v err=%v", reenqueue, recover, err)
	}
}

func TestBindingIsOneToOneAndIncreasing(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", "s6")
	a, _, _, _, _ := s.Enqueue("ui:alice", "", "a", "same", nil)
	b, _, _, _, _ := s.Enqueue("ui:alice", "", "b", "same", nil)
	for _, seq := range []int64{a, b} {
		if err := s.MarkRunMeta(seq, "codex", "S", "p", seq); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Bind(a, "codex", "S", 4, "r4"); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(b, "codex", "S", 4, "r4"); !errors.Is(err, ErrBindConflict) {
		t.Fatalf("coordinate reuse = %v", err)
	}
	if err := s.Bind(b, "codex", "S", 3, "r3"); !errors.Is(err, ErrBindConflict) {
		t.Fatalf("non-increasing bind = %v", err)
	}
	if err := s.Bind(b, "codex", "S", 6, "r6"); err != nil {
		t.Fatal(err)
	}
}

// The projection folds only appended records, sees another instance's appends, hands out
// copies, and re-folds from zero when the path holds another file.
func TestQueueProjectionFollowsAppends(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	a, b := Open("user:alice", "s12"), Open("user:alice", "s12")
	seq, _, _, _, err := a.Enqueue("ui:alice", "", "n1", "one", nil)
	if err != nil {
		t.Fatal(err)
	}
	if turns, _ := b.InboundLog(); len(turns) != 1 || turns[0].Last != "enq" {
		t.Fatalf("b before run = %+v", turns)
	}
	if err := a.MarkRunMeta(seq, "codex", "s", "d", 0); err != nil {
		t.Fatal(err)
	}
	if err := a.MarkHookError(seq, "audit.turn.finish", "x"); err != nil {
		t.Fatal(err)
	}
	turns, _ := b.InboundLog()
	if len(turns) != 1 || turns[0].Last != "run" || len(turns[0].HookFailures) != 1 {
		t.Fatalf("b after run = %+v", turns)
	}
	turns[0].Text, turns[0].HookFailures[0].Reason = "mutated", "mutated"
	if again, _ := b.InboundLog(); again[0].Text != "one" || again[0].HookFailures[0].Reason != "x" {
		t.Fatalf("caller mutation reached the projection: %+v", again[0])
	}
	if _, _, _, _, err := b.Enqueue("ui:alice", "", "n2", "two", nil); err != nil {
		t.Fatal(err)
	}
	if turns, _ := a.InboundLog(); len(turns) != 2 || turns[1].Seq != seq+1 {
		t.Fatalf("a after b's enqueue = %+v", turns)
	}

	path := a.queuePath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	enq := bytes.Index(data, []byte(`"ev":"enq"`))
	first := data[:enq+bytes.IndexByte(data[enq:], '\n')+1]
	if err := os.WriteFile(path+".new", first, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	if turns, _ := a.InboundLog(); len(turns) != 1 || turns[0].Last != "enq" {
		t.Fatalf("a after replacement = %+v", turns)
	}
}

// A complete record that lost its newline in a crash still counts: its seq is not reused.
func TestQueueUnterminatedCompleteRecordCounts(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	a := Open("user:alice", "s13")
	if _, _, _, _, err := a.Enqueue("ui:alice", "", "n1", "one", nil); err != nil {
		t.Fatal(err)
	}
	path := a.queuePath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.TrimRight(data, "\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := Open("user:alice", "s13")
	seq, _, _, _, err := b.Enqueue("ui:alice", "", "n2", "two", nil)
	if err != nil {
		t.Fatal(err)
	}
	turns, _ := b.InboundLog()
	if seq != 2 || len(turns) != 2 || turns[0].Text != "one" || turns[1].Text != "two" {
		t.Fatalf("seq %d, turns %+v", seq, turns)
	}
}
