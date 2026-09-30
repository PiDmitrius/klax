package history

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var codexFixture = []string{
	`{"type":"session_meta","payload":{"id":"t"}}`,
	`{"timestamp":"2026-09-30T10:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"first <!-- klax-turn:0123456789abcdef -->"}}`,
	`{"timestamp":"2026-09-30T10:00:01Z","type":"event_msg","payload":{"type":"agent_message","message":"thinking aloud"}}`,
	`{"timestamp":"2026-09-30T10:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":111},"model_context_window":1000}}}`,
	`{"timestamp":"2026-09-30T10:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}`,
	`{"timestamp":"2026-09-30T10:00:04Z","type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 3<<20) + `"}}`,
	`{"timestamp":"2026-09-30T10:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":222},"model_context_window":1000}}}`,
	`{"type":"compacted","payload":{}}`,
	`{"timestamp":"2026-09-30T10:00:06Z","type":"event_msg","payload":{"type":"context_compacted"}}`,
	`{"timestamp":"2026-09-30T10:00:07Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"text":"second"}]}}}`,
	`{"timestamp":"2026-09-30T10:00:08Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","content":[{"text":"answer"}]}}}`,
	`{"timestamp":"2026-09-30T10:00:09Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":333},"model_context_window":1000}}}`,
	`{"timestamp":"2026-09-30T10:00:10Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"boom"}}}`,
}

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

func fullLoad(t *testing.T, data string) transcriptSnapshot {
	t.Helper()
	path := filepath.Join(t.TempDir(), "full.jsonl")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := loadTranscript("codex", "s", path)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Appending at arbitrary byte boundaries, including inside a record, yields the
// same items and coordinates as indexing the finished file once, and never
// changes a snapshot already handed out.
func TestIncrementalIndexMatchesFullRead(t *testing.T) {
	data := strings.Join(codexFixture, "\n") + "\n"
	want := fullLoad(t, data)
	if len(want.items) != 7 || want.items[1].CtxUsed != 111 || want.items[2].CtxUsed != 222 {
		t.Fatalf("fixture items = %+v", want.items)
	}
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 20; round++ {
		path := filepath.Join(t.TempDir(), "grow.jsonl")
		type seen struct {
			snap  transcriptSnapshot
			items []Item
		}
		var history []seen
		for pos := 0; pos < len(data); {
			n := 1 + rng.Intn(len(data)/8)
			if rng.Intn(4) == 0 {
				n = 1 + rng.Intn(64)
			}
			n = min(n, len(data)-pos)
			appendFile(t, path, data[pos:pos+n])
			pos += n
			snap, err := loadTranscript("codex", "s", path)
			if err != nil {
				t.Fatal(err)
			}
			history = append(history, seen{snap, slices.Clone(snap.items)})
		}
		for _, h := range history {
			if h.snap.gen != history[0].snap.gen {
				t.Fatalf("round %d: an append changed the index generation", round)
			}
		}
		last := history[len(history)-1].snap
		if !reflect.DeepEqual(last.items, want.items) || !reflect.DeepEqual(last.ends, want.ends) {
			t.Fatalf("round %d: incremental index differs from full read", round)
		}
		for i, h := range history {
			if !reflect.DeepEqual(h.snap.items, h.items) {
				t.Fatalf("round %d: published snapshot %d was mutated", round, i)
			}
		}
	}
}

func TestIndexUnchangedFileIsNotReparsed(t *testing.T) {
	path := writeLines(t, codexFixture[:3])
	a, err := loadTranscript("codex", "s", path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := loadTranscript("codex", "s", path)
	if err != nil || &a.items[0] != &b.items[0] {
		t.Fatalf("unchanged file was re-indexed: %v", err)
	}
}

// Replacement and truncation re-index from zero instead of appending to stale state.
func TestIndexRebuildsChangedPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.jsonl")
	write := func(lines ...string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var gen uint64
	check := func(name string, lines ...string) {
		t.Helper()
		got, err := loadTranscript("codex", "s", path)
		if err != nil {
			t.Fatal(err)
		}
		if got.gen == gen {
			t.Fatalf("%s: re-index kept generation %d", name, gen)
		}
		gen = got.gen
		want := fullLoad(t, strings.Join(lines, "\n")+"\n")
		if !reflect.DeepEqual(got.items, want.items) || !reflect.DeepEqual(got.ends, want.ends) {
			t.Fatalf("%s: got %+v, want %+v", name, got.items, want.items)
		}
	}
	a, b, c := codexFixture[1], codexFixture[2], codexFixture[10]
	write(a, b)
	check("initial", a, b)

	write(a) // truncation
	check("truncated", a)

	tmp := filepath.Join(dir, "new.jsonl")
	if err := os.WriteFile(tmp, []byte(a+"\n"+c+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil { // replaced by another file
		t.Fatal(err)
	}
	check("replaced", a, c)
}

func TestCodexSilentRecognizesOnlyNonRenderingRecords(t *testing.T) {
	const lead = `{"timestamp":"2026-09-02T12:32:57.061Z","ordinal":44,`
	const ids = `"thread_id":"t","turn_id":"u",`
	for line, want := range map[string]bool{
		lead + `"type":"response_item","payload":{"type":"custom_tool_call_output","output":[]}}`:                      true,
		lead + `"type":"response_item","payload":{"type":"reasoning","encrypted_content":"x"}}`:                        true,
		lead + `"type":"event_msg","payload":{"type":"item_completed",` + ids + `"item":{"type":"CommandExecution"}}}`: true,
		lead + `"type":"event_msg","payload":{"type":"item_completed",` + ids + `"item":{"type":"UserMessage"}}}`:      false,
		lead + `"type":"event_msg","payload":{"type":"item_completed",` + ids + `"item":{"type":"AgentMessage"}}}`:     false,
		lead + `"type":"response_item","payload":{"type":"custom_tool_call","name":"exec"}}`:                           false,
		lead + `"type":"event_msg","payload":{"type":"token_count"}}`:                                                  false,
		`{"type":"response_item","payload":{"type":"reasoning"}}`:                                                      false,
	} {
		if got := codexSilent([]byte(line)); got != want {
			t.Errorf("codexSilent(%s) = %v, want %v", line, got, want)
		}
	}
}
