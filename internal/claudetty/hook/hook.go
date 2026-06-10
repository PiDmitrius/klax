// Package hook generates the Stop/SessionStart hook plumbing for a `claude`
// invocation: a per-run temp dir, a FIFO the parent reads, a tiny shell
// script that relays each hook payload to the FIFO, and the inline
// `--settings` JSON that tells `claude` to call it.
//
// Nothing here touches ~/.claude/ — all state lives under the temp dir and
// is removed by Harness.Close.
package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const scriptBody = `#!/bin/sh
# Relay a Claude Code hook event to claudetty's FIFO.
#   $1 = event name (e.g. "Stop", "SessionStart")
# stdin = the hook's JSON payload. It MUST stay a single line: ParseLine splits
# only the first tab and the driver scans the FIFO by newline. Claude emits
# compact JSON (newlines inside strings are escaped), so a multi-line
# last_assistant_message is still one physical line — do not pretty-print it.
set -eu
event="$1"
fifo="${CLAUDETTY_FIFO:?missing CLAUDETTY_FIFO}"
payload="$(cat)"
printf '%s\t%s\n' "$event" "$payload" >> "$fifo"
exit 0
`

// Harness is the per-run hook installation. Callers must Close it.
type Harness struct {
	TmpDir       string
	FifoPath     string
	ScriptPath   string
	SettingsJSON string
}

// New creates the temp dir, FIFO, relay script, and settings JSON.
func New() (*Harness, error) {
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("claudetty-%d-", os.Getpid()))
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	h := &Harness{
		TmpDir:     tmpDir,
		FifoPath:   tmpDir + "/events.fifo",
		ScriptPath: tmpDir + "/hook.sh",
	}
	if err := unix.Mkfifo(h.FifoPath, 0o600); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("mkfifo: %w", err)
	}
	if err := os.WriteFile(h.ScriptPath, []byte(scriptBody), 0o700); err != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("write hook script: %w", err)
	}
	h.SettingsJSON = settingsJSON(h.ScriptPath)
	return h, nil
}

// Close removes the temp dir and everything in it.
func (h *Harness) Close() {
	os.RemoveAll(h.TmpDir)
}

// settingsJSON builds the inline --settings document wiring SessionStart and
// Stop to the relay script.
func settingsJSON(scriptPath string) string {
	type hookCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Matcher string    `json:"matcher"`
		Hooks   []hookCmd `json:"hooks"`
	}
	events := map[string][]matcher{}
	for _, ev := range []string{"SessionStart", "Stop"} {
		// Single-quote the script path: claude runs the hook command through a
		// shell, so a space in TMPDIR would otherwise split the path. The path
		// comes from os.MkdirTemp (claudetty-<pid>-<rand>) and never contains a
		// single quote, so plain single-quoting is sufficient.
		events[ev] = []matcher{{
			Matcher: "*",
			Hooks:   []hookCmd{{Type: "command", Command: "'" + scriptPath + "' " + ev}},
		}}
	}
	doc := map[string]any{"hooks": events}
	b, _ := json.Marshal(doc)
	return string(b)
}

// Event is a parsed hook-relay line.
type Event struct {
	Name    string // "SessionStart", "Stop", ...
	Payload string // raw JSON payload
}

// ParseLine parses one "<event>\t<json>" line from the FIFO. Returns ok=false
// for malformed lines.
func ParseLine(line string) (Event, bool) {
	line = strings.TrimRight(line, "\r\n")
	name, payload, found := strings.Cut(line, "\t")
	if !found {
		return Event{}, false
	}
	return Event{Name: name, Payload: payload}, true
}

// payloadFields is the subset of hook payload fields claudetty reads.
type payloadFields struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

// ExtractFields pulls the known fields out of a hook payload. Missing fields
// come back empty — callers decide what is required.
func ExtractFields(payload string) (sessionID, transcriptPath, lastAssistantMessage string) {
	var f payloadFields
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return "", "", ""
	}
	return f.SessionID, f.TranscriptPath, f.LastAssistantMessage
}
