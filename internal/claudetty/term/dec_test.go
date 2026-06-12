package term

import (
	"bytes"
	"testing"
)

func TestDA1(t *testing.T) {
	got := RespondToDecQueries([]byte("\x1b[c"), 40, 120)
	if string(got) != "\x1b[?1;2c" {
		t.Fatalf("DA1 = %q", got)
	}
}

func TestDA2(t *testing.T) {
	got := RespondToDecQueries([]byte("\x1b[>c"), 40, 120)
	if string(got) != "\x1b[>0;0;0c" {
		t.Fatalf("DA2 = %q", got)
	}
}

func TestDSRCursor(t *testing.T) {
	got := RespondToDecQueries([]byte("\x1b[6n"), 40, 120)
	if string(got) != "\x1b[1;1R" {
		t.Fatalf("DSR = %q", got)
	}
}

func TestXTVersion(t *testing.T) {
	got := RespondToDecQueries([]byte("\x1b[>q"), 40, 120)
	if !bytes.HasPrefix(got, []byte("\x1bP>|claudetty")) || !bytes.HasSuffix(got, []byte("\x1b\\")) {
		t.Fatalf("XTVERSION = %q", got)
	}
}

func TestWindowSize(t *testing.T) {
	got := RespondToDecQueries([]byte("\x1b[18t"), 40, 120)
	if string(got) != "\x1b[8;40;120t" {
		t.Fatalf("winsize = %q", got)
	}
	// The report must follow the actual PTY size, not a baked-in default.
	got = RespondToDecQueries([]byte("\x1b[18t"), 50, 200)
	if string(got) != "\x1b[8;50;200t" {
		t.Fatalf("winsize 50x200 = %q", got)
	}
}

func TestIgnoresPlainText(t *testing.T) {
	if got := RespondToDecQueries([]byte("hello world without esc"), 40, 120); len(got) != 0 {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestMultipleQueriesInOneChunk(t *testing.T) {
	got := RespondToDecQueries([]byte("hi\x1b[cthere\x1b[>cyo"), 40, 120)
	if !bytes.Contains(got, []byte("\x1b[?1;2c")) || !bytes.Contains(got, []byte("\x1b[>0;0;0c")) {
		t.Fatalf("got %q", got)
	}
}

func TestOtherCSIIgnored(t *testing.T) {
	// Cursor movement, SGR colors — no response expected.
	if got := RespondToDecQueries([]byte("\x1b[1C\x1b[38;5;208m\x1b[2J"), 40, 120); len(got) != 0 {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestStripEscapes(t *testing.T) {
	in := []byte("Do you \x1b[1Ctrust\x1b[38;5;2m this \x1b]0;title\x07folder\x1b[0m?")
	got := string(StripEscapes(in))
	if got != "Do you trust this folder?" {
		t.Fatalf("stripped = %q", got)
	}
}

func TestStripEscapesDCS(t *testing.T) {
	in := []byte("a\x1bP>|something\x1b\\b")
	if got := string(StripEscapes(in)); got != "ab" {
		t.Fatalf("stripped = %q", got)
	}
}
