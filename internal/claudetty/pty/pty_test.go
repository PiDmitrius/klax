package pty

import (
	"os"
	"testing"
)

// TestOpenRoundTrip allocates a real pty pair and verifies bytes written to the
// slave surface on the master. This exercises the platform-specific allocation
// (TIOCGPTN on Linux, TIOCPTYGNAME/grant/unlock on macOS).
func TestOpenRoundTrip(t *testing.T) {
	p, err := Open(24, 80)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	if p.Master == nil || p.Slave == nil {
		t.Fatal("Open returned nil master or slave")
	}

	const msg = "klax"
	if _, err := p.Slave.Write([]byte(msg)); err != nil {
		t.Fatalf("write slave: %v", err)
	}

	buf := make([]byte, len(msg))
	if _, err := readFull(p.Master, buf); err != nil {
		t.Fatalf("read master: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("round trip: got %q, want %q", buf, msg)
	}
}

// readFull reads len(buf) bytes, looping over short reads from the pty master.
func readFull(f *os.File, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := f.Read(buf[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}
