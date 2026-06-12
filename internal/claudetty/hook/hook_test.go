package hook

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSettingsJSONShape(t *testing.T) {
	js := settingsJSON("/tmp/hook.sh")
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(js), &doc); err != nil {
		t.Fatalf("settings JSON invalid: %v", err)
	}
	for _, ev := range []string{"SessionStart", "Stop"} {
		ms, ok := doc.Hooks[ev]
		if !ok || len(ms) == 0 {
			t.Fatalf("missing %s hook", ev)
		}
		if ms[0].Matcher != "*" {
			t.Fatalf("%s matcher = %q", ev, ms[0].Matcher)
		}
		cmd := ms[0].Hooks[0]
		// The script path is single-quoted so a space in TMPDIR can't split it.
		if cmd.Type != "command" || cmd.Command != "'/tmp/hook.sh' "+ev {
			t.Fatalf("%s command = %+v", ev, cmd)
		}
	}
}

func TestParseLine(t *testing.T) {
	ev, ok := ParseLine("Stop\t{\"transcript_path\":\"/tmp/x.jsonl\"}\n")
	if !ok || ev.Name != "Stop" || ev.Payload != `{"transcript_path":"/tmp/x.jsonl"}` {
		t.Fatalf("got %+v ok=%v", ev, ok)
	}
	if _, ok := ParseLine("no-tab-here"); ok {
		t.Fatal("malformed line accepted")
	}
}

func TestExtractFields(t *testing.T) {
	sid, tp, last := ExtractFields(`{"session_id":"abc","transcript_path":"/a/b.jsonl","last_assistant_message":"OK"}`)
	if sid != "abc" || tp != "/a/b.jsonl" || last != "OK" {
		t.Fatalf("got %q %q %q", sid, tp, last)
	}
	sid, tp, last = ExtractFields("not-json")
	if sid != "" || tp != "" || last != "" {
		t.Fatal("malformed payload should yield empties")
	}
}

func TestHarnessRoundTrip(t *testing.T) {
	h, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if _, err := os.Stat(h.ScriptPath); err != nil {
		t.Fatalf("script missing: %v", err)
	}
	st, err := os.Stat(h.FifoPath)
	if err != nil {
		t.Fatalf("fifo missing: %v", err)
	}
	if st.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("fifo is not a named pipe")
	}

	// The relay script writes "<event>\t<payload>" to the FIFO.
	fd, err := unix.Open(h.FifoPath, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	cmdOut, err := os.ReadFile(h.ScriptPath)
	if err != nil || !strings.Contains(string(cmdOut), "CLAUDETTY_FIFO") {
		t.Fatalf("script content: %v %q", err, cmdOut)
	}

	h.Close()
	if _, err := os.Stat(h.TmpDir); !os.IsNotExist(err) {
		t.Fatal("Close did not remove tmp dir")
	}
}

func TestSweepStaleTmpDirs(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string) string {
		p := dir + "/" + name
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+"/hook.sh", []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dead := mk("claudetty-111-aaa")
	live := mk("claudetty-222-bbb")
	foreign := mk("unrelated-111-ccc")
	noPid := mk("claudetty-nopid")
	badPid := mk("claudetty-xyz-ddd")
	plainFile := dir + "/claudetty-333-file"
	if err := os.WriteFile(plainFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	removed := sweepStaleTmpDirs(dir, func(pid int) bool { return pid == 222 })
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatal("dead wrapper's dir survived the sweep")
	}
	for _, p := range []string{live, foreign, noPid, badPid, plainFile} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("sweep touched %s: %v", p, err)
		}
	}
}
