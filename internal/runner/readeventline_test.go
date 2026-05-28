package runner

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// TestReadEventLineDrainsOversizedLine is the regression guard for the codex
// hang: a single JSON event larger than any buffer must not stop the read loop.
// The reader has to keep consuming past the giant line so the backend can flush
// the rest of its output and exit (otherwise cmd.Wait deadlocks on a full pipe).
func TestReadEventLineDrainsOversizedLine(t *testing.T) {
	huge := strings.Repeat("x", maxEventLine+50_000)
	input := huge + "\n" + `{"type":"turn.completed"}` + "\n"
	r := bufio.NewReaderSize(strings.NewReader(input), 64*1024)

	first, err := readEventLine(r)
	if err != nil {
		t.Fatalf("first line: unexpected error %v", err)
	}
	if len(first) != maxEventLine {
		t.Fatalf("oversized line not truncated to cap: got %d want %d", len(first), maxEventLine)
	}

	second, err := readEventLine(r)
	if err != nil {
		t.Fatalf("second line: unexpected error %v", err)
	}
	if string(second) != `{"type":"turn.completed"}` {
		t.Fatalf("stream did not keep draining after oversized line: got %q", second)
	}

	if _, err := readEventLine(r); err != io.EOF {
		t.Fatalf("expected io.EOF at end of stream, got %v", err)
	}
}

func TestReadEventLineStripsTerminators(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("a\r\nb\nc"), 64*1024)
	for _, want := range []string{"a", "b", "c"} {
		got, _ := readEventLine(r)
		if string(got) != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}

// TestReadEventLineFinalUnterminatedLine: a non-empty last line with no '\n'
// must be returned together with io.EOF (not dropped), and the next call must
// then report a clean empty io.EOF so the Run loop terminates.
func TestReadEventLineFinalUnterminatedLine(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("first\nlast-no-newline"), 64*1024)

	if got, err := readEventLine(r); string(got) != "first" || err != nil {
		t.Fatalf("line 1 = %q, %v; want \"first\", nil", got, err)
	}
	got, err := readEventLine(r)
	if string(got) != "last-no-newline" || err != io.EOF {
		t.Fatalf("final line = %q, %v; want \"last-no-newline\", io.EOF", got, err)
	}
	if got, err := readEventLine(r); len(got) != 0 || err != io.EOF {
		t.Fatalf("post-EOF = %q, %v; want empty, io.EOF", got, err)
	}
}

// TestReadEventLineExactCap: a line of exactly maxEventLine bytes followed by a
// newline is returned intact; the following line is unaffected.
func TestReadEventLineExactCap(t *testing.T) {
	exact := strings.Repeat("y", maxEventLine)
	r := bufio.NewReaderSize(strings.NewReader(exact+"\nnext\n"), 64*1024)

	got, err := readEventLine(r)
	if err != nil || len(got) != maxEventLine {
		t.Fatalf("exact-cap line = len %d, %v; want len %d, nil", len(got), err, maxEventLine)
	}
	if got, _ := readEventLine(r); string(got) != "next" {
		t.Fatalf("following line = %q; want \"next\"", got)
	}
}

// TestReadEventLineCapWithCRLF: when payload+"\r\n" is exactly maxEventLine
// bytes the whole terminator fits within the cap, so both "\r" and "\n" are
// stripped and the payload comes back intact. (A terminator that straddles the
// cap is intentionally left bounded — that case is not the payload-intact path.)
func TestReadEventLineCapWithCRLF(t *testing.T) {
	payload := strings.Repeat("z", maxEventLine-2)
	r := bufio.NewReaderSize(strings.NewReader(payload+"\r\n"), 64*1024)

	got, err := readEventLine(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("payload corrupted: len %d (want %d)", len(got), len(payload))
	}
}

// TestReadEventLineOversizedNoFinalNewline: an over-cap line with no trailing
// newline must still drain to EOF and return the capped prefix with io.EOF —
// the worst case for the wedge bug (giant final event, stream ends mid-write).
func TestReadEventLineOversizedNoFinalNewline(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader(strings.Repeat("q", maxEventLine+12_345)), 64*1024)

	got, err := readEventLine(r)
	if err != io.EOF {
		t.Fatalf("err = %v; want io.EOF", err)
	}
	if len(got) != maxEventLine {
		t.Fatalf("len = %d; want capped to %d", len(got), maxEventLine)
	}
}
