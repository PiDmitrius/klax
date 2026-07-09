package sessfiles

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// An aborted queued turn (enq -> err:aborted, never run) must NOT replay.
func TestAbortedTurnDoesNotReplay(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", 9)
	a, _, _, _, _ := s.Enqueue("tg:1", "", "a", "A", nil)
	b, _, _, _, _ := s.Enqueue("tg:1", "", "b", "B", nil)
	if err := s.MarkErr(a, "aborted"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkErr(b, "aborted"); err != nil {
		t.Fatal(err)
	}
	reenq, recovered, err := Open("user:alice", 9).Replay()
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
	s := Open("user:alice", 11)
	seq, _, _, _, _ := s.Enqueue("tg:1", "", "n", "hi", nil)
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("dir should be gone after Remove")
	}
	if err := s.MarkDone(seq); !errors.Is(err, ErrRemoved) {
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

// Enqueue allocates an increasing turn_seq, returns a non-empty unique marker,
// persists files + an enq record, and the seq survives a "restart" (fresh Store
// reading the same log) — continuing from the log, not from 1.
func TestEnqueueAllocatesAndSurvivesRestart(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s := Open("user:alice", 100)
	seq1, m1, f1, _, err := s.Enqueue("ui:alice", "", "n1", "hi", []NamedReader{nr("a.png", "AA")})
	if err != nil {
		t.Fatal(err)
	}
	seq2, m2, _, _, _ := s.Enqueue("ui:alice", "", "n2", "yo", nil) // text-only turn is fine
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seqs = %d,%d want 1,2", seq1, seq2)
	}
	if m1 == "" || m2 == "" || m1 == m2 {
		t.Fatalf("markers must be non-empty and distinct: %q %q", m1, m2)
	}
	if len(f1) != 1 || f1[0] != "000001-01-a.png" {
		t.Fatalf("stored = %v want [000001-01-a.png]", f1)
	}
	if b, _ := os.ReadFile(s.Path(f1[0])); string(b) != "AA" {
		t.Fatalf("file bytes = %q want AA", b)
	}
	// Restart: a fresh Store reads the durable log and continues the sequence.
	s2 := Open("user:alice", 100)
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
	s := Open("user:alice", 101)
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
	s := Open("user:alice", 7)
	sA, _, _, _, _ := s.Enqueue("tg:1", "", "a", "A", nil)
	s.MarkRun(sA)
	s.MarkDone(sA)                       // A: complete → skipped
	s.Enqueue("tg:1", "", "b", "B", nil) // B: enq only → reenqueue
	sC, _, _, _, _ := s.Enqueue("tg:1", "", "c", "C", nil)
	s.MarkRun(sC) // C: run, no terminal → recover

	reenq, recover, err := Open("user:alice", 7).Replay()
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
	s := Open("user:alice", 1)
	s.Enqueue("tg:1", "", "n", "good", nil)
	// Simulate a crash mid-append: a partial JSON line with no newline.
	f, _ := os.OpenFile(s.queuePath(), os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(`{"ev":"enq","seq":2,"text":"tor`)
	f.Close()

	log, err := Open("user:alice", 1).InboundLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Text != "good" {
		t.Fatalf("InboundLog = %+v, want one clean 'good' turn", log)
	}
}
