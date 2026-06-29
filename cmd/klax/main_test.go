package main

import (
	"os"
	"testing"
)

// TestMain isolates every cmd/klax test from the real klax data dir. The durable
// session store (sessfiles) resolves its root from KLAX_DATA_DIR, so without this a
// test that enqueues would write under the user's real ~/.local/share/klax and
// could collide with a running daemon.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "klax-test-data-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("KLAX_DATA_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
