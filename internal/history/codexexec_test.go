package history

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDecodeCodexExecTools(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantName  string // expected single tool Name; "" means expect nil (fallback)
		wantLabel string // substring the label must contain (when wantName != "")
	}{
		{
			name:      "exec_command quoted keys",
			input:     `const r = await tools.exec_command({"cmd":"brave-search 'CVE-2026-53359'","workdir":"/h","yield_time_ms":10000}); text(r.output);`,
			wantName:  "Exec",
			wantLabel: "brave-search 'CVE-2026-53359'",
		},
		{
			name:      "exec_command unquoted keys",
			input:     `const r = await tools.exec_command({cmd:"sed -n '1,240p' /etc/hosts"}); text(r.output)`,
			wantName:  "Exec",
			wantLabel: "sed -n '1,240p' /etc/hosts",
		},
		{
			name:      "exec_command with escaped backslashes",
			input:     `const r = await tools.exec_command({cmd:"find ~ -type f \\( -name '*.jsonl' \\)"}); text(r.output)`,
			wantName:  "Exec",
			wantLabel: `find ~ -type f \( -name '*.jsonl' \)`,
		},
		{
			name:      "view_image",
			input:     `const a = await tools.view_image({path:"/tmp/klax-attach/image.png", detail:"original"}); image(a.image_url);`,
			wantName:  "ViewImage",
			wantLabel: "/tmp/klax-attach/image.png",
		},
		{
			name:      "apply_patch via const alias (add file -> Write)",
			input:     "const patch = \"*** Begin Patch\\n*** Add File: /tmp/note.md\\n+hi\\n*** End Patch\";\nconst r = await tools.apply_patch(patch); text(r.output);",
			wantName:  "Write",
			wantLabel: "/tmp/note.md",
		},
		{
			name:      "web__run",
			input:     `const r = await tools.web__run({"open":[{"ref_id":"https://x/y"}],"response_length":"long"}); text(JSON.stringify(r));`,
			wantName:  "Web",
			wantLabel: "🌐 Web",
		},
		{
			name:      "write_stdin poll (empty chars) -> Wait, no session id",
			input:     `const r = await tools.write_stdin({session_id: 12345, chars: ""}); text(r.output);`,
			wantName:  "Wait",
			wantLabel: "⏳ Wait",
		},
		{
			name:      "write_stdin with input chars -> Exec input",
			input:     `const r = await tools.write_stdin({session_id: 7, chars: "y\n"}); text(r.output);`,
			wantName:  "Exec",
			wantLabel: "ввод: y",
		},
		{
			name:      "string-aware: tools.* inside the command is not a call",
			input:     `const r = await tools.exec_command({cmd:"echo 'await tools.exec_command(fake)'"}); text(r.output);`,
			wantName:  "Exec",
			wantLabel: "echo 'await tools.exec_command(fake)'",
		},
		{
			name:      "unknown nested tool stays visible",
			input:     `const r = await tools.frobnicate({x:1}); text(r);`,
			wantName:  "frobnicate",
			wantLabel: "🔧 frobnicate",
		},
		{
			name:     "no tools.* call -> nil (keep exec fallback)",
			input:    `text("just a message"); notify("done");`,
			wantName: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := decodeCodexExecTools(tc.input)
			if tc.wantName == "" {
				if rows != nil {
					t.Fatalf("want nil (fallback), got %+v", rows)
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("want 1 tool, got %d: %+v", len(rows), rows)
			}
			got := []ToolCall{rows[0].tool}
			if got[0].Name != tc.wantName {
				t.Fatalf("name = %q, want %q (label %q)", got[0].Name, tc.wantName, got[0].Label)
			}
			if !strings.Contains(got[0].Label, tc.wantLabel) {
				t.Fatalf("label %q must contain %q", got[0].Label, tc.wantLabel)
			}
		})
	}
}

// The wrapper must decode through readCodex end-to-end, replacing the opaque 🔧 exec.
func TestReadCodexDecodesExecWrapper(t *testing.T) {
	path := writeLines(t, []string{
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"const r = await tools.exec_command({cmd:\"pwd\"}); text(r.output);"}}`,
	})
	items, err := readCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Tools) != 1 {
		t.Fatalf("want 1 tool item, got %+v", items)
	}
	if tc := items[0].Tools[0]; tc.Name != "Exec" || !strings.Contains(tc.Label, "pwd") {
		t.Fatalf("exec wrapper not decoded: %+v", tc)
	}
}

// Every write_stdin poll (empty chars) is a real Wait progress event. None are collapsed, and the
// internal session id remains hidden.
func TestReadCodexWriteStdinPollsStayVisible(t *testing.T) {
	poll := `{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"await tools.write_stdin({session_id: 9, chars: \"\"});"}}`
	cmd := `{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"await tools.exec_command({cmd:\"ls\"});"}}`

	// Three consecutive polls remain three honest progress rows.
	items, err := readCodex(writeLines(t, []string{poll, poll, poll}))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("consecutive polls must remain visible, got %d: %+v", len(items), items)
	}
	for _, item := range items {
		if len(item.Tools) != 1 || item.Tools[0].Name != "Wait" || item.Tools[0].Label != "⏳ Wait" {
			t.Fatalf("want canonical Wait row, got %+v", item)
		}
		if strings.Contains(item.Tools[0].Label, "9") || strings.Contains(item.Tools[0].Label, "session") {
			t.Fatalf("wait row must not expose the internal session id: %q", item.Tools[0].Label)
		}
	}

	// A real command between polls preserves every record in order.
	items, err = readCodex(writeLines(t, []string{poll, poll, cmd, poll}))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("poll,poll,cmd,poll must yield four rows, got %d: %+v", len(items), items)
	}
}

// An undecodable wrapper shows the raw orchestration source as an Exec row (never dropped,
// never opaque) so the user still sees what Codex ran.
func TestReadCodexExecFallbackShowsRawAsExec(t *testing.T) {
	path := writeLines(t, []string{
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"yield_control();"}}`,
	})
	items, err := readCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(items[0].Tools) != 1 {
		t.Fatalf("want single fallback row, got %+v", items)
	}
	if tc := items[0].Tools[0]; tc.Name != "Exec" || !strings.Contains(tc.Label, "yield_control") {
		t.Fatalf("want raw source shown as Exec, got %+v", tc)
	}
}

const batchSrc = `const calls = [{cmd:"pwd", workdir:"/work"}, {cmd:"false", workdir:"/work"}]; await Promise.allSettled(calls.map(x => tools.exec_command(x)))`

func TestDecodeCodexExecBatch(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"const array", batchSrc, "pwd|false"},
		{"inline array, async parenthesized", `await Promise.all([{"cmd":"a"},{cmd:'b'}].map(async (c) => await tools.exec_command(c)));`, "a|b"},
		{"element not an object", `const calls = ["pwd"]; calls.map(x => tools.exec_command(x))`, "?"},
		{"callback changes the argument", `const calls = [{cmd:"pwd"}]; calls.map(x => tools.exec_command({...x, yield_time_ms: 1}))`, "?"},
		{"array of direct calls", `await Promise.allSettled([tools.exec_command({cmd:"a"}), tools.exec_command({cmd:"b"})])`, "a|b"},
		{"concatenated command", `await tools.exec_command({cmd: "ssh " + host, workdir: "/w"})`, "?"},
		{"array continued on the next line", "const calls = [{cmd:\"pwd\"}]\n  .slice(0, 0); calls.map(x => tools.exec_command(x))", "?"},
		{"array before the next statement", "const calls = [{cmd:\"pwd\"}]\nawait Promise.all(calls.map(x => tools.exec_command(x)))", "pwd"},
		{"array not the whole initializer", `const calls = [{cmd:"pwd"}].slice(0, 0); calls.map(x => tools.exec_command(x))`, "?"},
		{"interpolating template", "await Promise.all(files.map(f => tools.exec_command({cmd: `cat ${f}`})))", "?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range decodeCodexExecTools(tc.src) {
				if !r.exec {
					t.Fatalf("non-exec row %+v", r)
				}
				if r.cmd == "" {
					if r.tool.Label == "" {
						t.Fatalf("undecoded row is invisible: %+v", r)
					}
					got = append(got, "?")
					continue
				}
				if !strings.Contains(r.tool.Label, "`"+r.cmd+"`") {
					t.Fatalf("row label %q lacks %q", r.tool.Label, r.cmd)
				}
				got = append(got, r.cmd)
			}
			if strings.Join(got, "|") != tc.want {
				t.Fatalf("commands = %q, want %q", got, tc.want)
			}
		})
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

var execT0 = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func execStamp(ms int) string {
	return execT0.Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339Nano)
}

func execUser(ms int, text string) string {
	return `{"timestamp":"` + execStamp(ms) + `","type":"event_msg","payload":{"type":"user_message","message":` + jsonString(text) + `}}`
}

func execCall(ms int, id, src string) string {
	return `{"timestamp":"` + execStamp(ms) + `","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"` + id + `","name":"exec","input":` + jsonString(src) + `}}`
}

func execOutput(ms int, id string) string {
	return `{"timestamp":"` + execStamp(ms) + `","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"` + id + `","output":[{"type":"input_text","text":"x"}]}}`
}

// execDone is a completed command: it finished at ms after running for durMs.
// A negative durMs leaves the duration out.
func execDone(ms, durMs int, cmd, status string, exit int, output string) string {
	argv, _ := json.Marshal([]string{"/bin/bash", "-lc", cmd})
	duration := fmt.Sprintf(`,"duration":{"secs":%d,"nanos":%d}`, durMs/1000, durMs%1000*int(time.Millisecond))
	if durMs < 0 {
		duration = ""
	}
	return fmt.Sprintf(`{"timestamp":"%s","type":"event_msg","payload":{"type":"item_completed","thread_id":"t","turn_id":"u","item":{"type":"CommandExecution","id":"exec-%d","command":%s,"cwd":"file:///work","status":"%s","aggregated_output":%s,"exit_code":%d%s},"completed_at_ms":%d}}`,
		execStamp(ms), ms, argv, status, jsonString(output), exit, duration, execT0.Add(time.Duration(ms)*time.Millisecond).UnixMilli())
}

func execTools(t *testing.T, lines []string) [][]ToolCall {
	t.Helper()
	items, err := readCodex(writeLines(t, lines))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]ToolCall
	for _, it := range items {
		if it.Role == "assistant" {
			out = append(out, it.Tools)
		}
	}
	return out
}

func labels(tools []ToolCall) []string {
	var out []string
	for _, tc := range tools {
		out = append(out, tc.Label)
	}
	return out
}

var batchLines = []string{
	execUser(0, "go"),
	execCall(100, "call_1", batchSrc),
	execDone(130, 20, "false", "failed", 1, "boom\n"),
	execDone(150, 40, "pwd", "completed", 0, "/work\n"),
	execOutput(160, "call_1"),
}

// A batch the source hides behind a variable shows its commands from the completion events:
// in source order, with the failure marked and directory, outcome and output in the detail.
func TestCodexExecBatchUsesCompletedCommands(t *testing.T) {
	got := execTools(t, batchLines)
	if len(got) != 1 {
		t.Fatalf("want one wrapper item, got %+v", got)
	}
	want := []string{"⚙️ Exec: `pwd`", "⚙️ Exec: `false` ✗ (exit 1)"}
	if !reflect.DeepEqual(labels(got[0]), want) {
		t.Fatalf("labels = %q, want %q", labels(got[0]), want)
	}
	if d := got[0][0].Detail; d != "$ pwd\ncwd: /work\nstatus: completed, exit 0\n\n/work" {
		t.Fatalf("pwd detail = %q", d)
	}
	if d := got[0][1].Detail; d != "$ false\ncwd: /work\nstatus: failed, exit 1\n\nboom" {
		t.Fatalf("false detail = %q", d)
	}
}

// Without completion events the statically decoded rows stay as they are.
func TestCodexExecBatchWithoutEvents(t *testing.T) {
	got := execTools(t, []string{execUser(0, "go"), execCall(100, "call_1", batchSrc), execOutput(160, "call_1")})
	if want := []string{"⚙️ Exec: `pwd`", "⚙️ Exec: `false`"}; len(got) != 1 || !reflect.DeepEqual(labels(got[0]), want) {
		t.Fatalf("got %+v, want %q", got, want)
	}
	if got[0][0].Detail != "" || got[0][1].Detail != "" {
		t.Fatalf("static rows carry a detail: %+v", got[0])
	}
}

func TestCodexExecSingleLiteralCompleted(t *testing.T) {
	got := execTools(t, []string{
		execUser(0, "go"),
		execCall(100, "call_1", `const r = await tools.exec_command({cmd:"ls -la", workdir:"/work"}); text(r.output);`),
		execDone(140, 30, "ls -la", "completed", 0, ""),
		execOutput(150, "call_1"),
	})
	if len(got) != 1 || len(got[0]) != 1 || got[0][0].Label != "⚙️ Exec: `ls -la`" || got[0][0].Detail != "$ ls -la\ncwd: /work\nstatus: completed, exit 0" {
		t.Fatalf("got %+v", got)
	}
}

// An action whose argument does not decode stays a visible row until its commands complete,
// then shows them; a source with no tools.* call keeps its raw fallback the same way.
func TestCodexExecUnrecognizedFormTakesEvents(t *testing.T) {
	loop := `for (const c of cmds) await tools.exec_command({cmd: c});`
	got := execTools(t, []string{execUser(0, "go"), execCall(100, "call_1", loop)})
	if len(got) != 1 || !reflect.DeepEqual(labels(got[0]), []string{"🔧 exec_command"}) {
		t.Fatalf("pending loop = %+v", got)
	}
	got = execTools(t, []string{
		execUser(0, "go"),
		execCall(100, "call_1", loop),
		execDone(120, 10, "a", "completed", 0, ""),
		execDone(140, 10, "b", "completed", 0, ""),
		execOutput(150, "call_1"),
		execCall(200, "call_2", "yield_control();"),
		execDone(220, 10, "c", "completed", 0, ""),
		execOutput(230, "call_2"),
	})
	want := [][]string{{"⚙️ Exec: `a`", "⚙️ Exec: `b`"}, {"⚙️ Exec: `c`"}}
	if len(got) != 2 || !reflect.DeepEqual(labels(got[0]), want[0]) || !reflect.DeepEqual(labels(got[1]), want[1]) {
		t.Fatalf("got %+v, want %q", got, want)
	}
}

// A command completes the row of the wrapper that started it: a long command finishing under a
// later wrapper, a command no wrapper started, and a command of an earlier turn change nothing
// else, and other tools' rows are never replaced.
func TestCodexExecEventsStayInTheirWrapper(t *testing.T) {
	got := execTools(t, []string{
		execUser(0, "go"),
		execCall(100, "call_1", `text(await tools.exec_command({cmd:"sleep 9"}))`),
		execOutput(1100, "call_1"), // yielded while running
		execCall(2000, "call_2", `await tools.write_stdin({session_id: 1, chars: ""}); await tools.exec_command({cmd:"ls"})`),
		execDone(2100, 50, "ls", "completed", 0, ""),
		execDone(2200, 2090, "sleep 9", "failed", 130, ""),
		execOutput(2300, "call_2"),
		`{"timestamp":"` + execStamp(3000) + `","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"date\"}"}}`,
		execDone(3100, 50, "date", "completed", 0, ""),
		execUser(4000, "next"),
		execCall(4100, "call_3", `await tools.write_stdin({session_id: 2, chars: ""})`),
		execDone(4200, 3000, "make", "completed", 0, ""), // started in the previous turn
		execOutput(4300, "call_3"),
	})
	want := [][]string{
		{"⚙️ Exec: `sleep 9` ✗ (exit 130)"},
		{"⏳ Wait", "⚙️ Exec: `ls`"},
		{"⚙️ Exec: `date`"},
		{"⏳ Wait"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %q", got, want)
	}
	for i := range want {
		if !reflect.DeepEqual(labels(got[i]), want[i]) {
			t.Fatalf("item %d = %q, want %q", i, labels(got[i]), want[i])
		}
	}
	if got[2][0].Detail != "" {
		t.Fatalf("a direct call took a wrapper's event: %+v", got[2])
	}
}

// Undecoded rows take the commands their literal parts match, a template up to its literal
// suffix. Further commands are appended after the rows already shown, which keep their places.
func TestCodexExecUndecodedRowsMatchTheirParts(t *testing.T) {
	src := "for (const h of hosts) await tools.exec_command({cmd: `run ${h} set ${k}`});\n" +
		"for (const h of hosts) await tools.exec_command({cmd: `run ${h} get`});\n" +
		`await tools.exec_command({cmd: "ssh " + host}); await tools.view_image({path: "/tmp/a.png"});`
	got := execTools(t, []string{
		execUser(0, "go"),
		execCall(100, "call_1", src),
		execDone(110, 5, "run a set x", "completed", 0, ""),
		execDone(120, 5, "run b set y", "completed", 0, ""),
		execDone(130, 5, "run a get", "completed", 0, ""),
		execDone(140, 5, "ssh h1", "completed", 0, ""),
		execDone(150, 5, "run b get", "completed", 0, ""),
		execOutput(160, "call_1"),
	})
	want := []string{"⚙️ Exec: `run a set x`", "⚙️ Exec: `run a get`", "⚙️ Exec: `ssh h1`", "🖼️ Image: /tmp/a.png", "⚙️ Exec: `run b set y`", "⚙️ Exec: `run b get`"}
	if len(got) != 1 || !reflect.DeepEqual(labels(got[0]), want) {
		t.Fatalf("got %q, want %q", labels(got[0]), want)
	}
}

// A template the decoder cuts short — a nested template, an escaped ${ — leaves its end open
// and takes its command instead of failing the read.
func TestCodexExecTemplateOpenEnd(t *testing.T) {
	for _, tc := range []struct{ src, cmd string }{
		{"const r = await tools.exec_command({cmd:`echo ${xs.map(x=>`,${x}`).join(\"\")}`});", "echo ,a,b"},
		{"await tools.exec_command({cmd: `echo '\\${'`});", "echo '${'"},
	} {
		got := execTools(t, []string{
			execUser(0, "go"),
			execCall(100, "call_1", tc.src),
			execDone(110, 5, tc.cmd, "completed", 0, ""),
			execOutput(120, "call_1"),
		})
		if want := []string{"⚙️ Exec: `" + tc.cmd + "`"}; len(got) != 1 || !reflect.DeepEqual(labels(got[0]), want) {
			t.Fatalf("%s: got %q, want %q", tc.src, got, want)
		}
	}
}

// A template's literal suffix keeps a longer neighbour's command out of its row.
func TestCodexExecTemplateSuffix(t *testing.T) {
	src := "await tools.exec_command({cmd: `run ${h} get`}); await tools.exec_command({cmd: `run ${h} get all`});"
	got := execTools(t, []string{
		execUser(0, "go"),
		execCall(100, "call_1", src),
		execDone(110, 5, "run a get all", "completed", 0, ""),
		execDone(120, 5, "run a get", "completed", 0, ""),
		execOutput(130, "call_1"),
	})
	if want := []string{"⚙️ Exec: `run a get`", "⚙️ Exec: `run a get all`"}; len(got) != 1 || !reflect.DeepEqual(labels(got[0]), want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Once a later item follows a wrapper, its rows are only replaced: a further command for a
// filled row or one the source does not show is dropped instead of shifting later blocks. A
// command without a duration has no known start and changes nothing. A wrapper whose window
// encloses another still takes the commands started after the inner one closed.
func TestCodexExecLateCommandsDoNotShiftBlocks(t *testing.T) {
	got := execTools(t, []string{
		execUser(0, "go"),
		execCall(100, "call_1", `await tools.exec_command({cmd:"make"}); for (const f of fs) await tools.exec_command({cmd: f});`),
		execCall(200, "call_2", `await tools.exec_command({cmd:"ls"})`),
		execDone(250, 10, "ls", "completed", 0, ""),
		execOutput(300, "call_2"),
		execDone(360, 10, "a", "completed", 0, ""), // started inside call_1 only
		execDone(370, 20, "b", "completed", 0, ""), // a second command for the filled row
		execDone(380, -1, "ls", "completed", 0, ""),
		execDone(390, 280, "make", "failed", 2, ""), // fills its own row in place
		execOutput(400, "call_1"),
	})
	want := [][]string{{"⚙️ Exec: `make` ✗ (exit 2)", "⚙️ Exec: `a`"}, {"⚙️ Exec: `ls`"}}
	if len(got) != 2 || !reflect.DeepEqual(labels(got[0]), want[0]) || !reflect.DeepEqual(labels(got[1]), want[1]) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Appending the batch record by record and at arbitrary byte boundaries shows the rows as soon
// as the wrapper lands, completes them without duplicates, never mutates a published snapshot,
// and ends where indexing the finished file once ends.
func TestCodexExecIncrementalAppend(t *testing.T) {
	data := strings.Join(batchLines, "\n") + "\n"
	want := fullLoad(t, data)
	path := filepath.Join(t.TempDir(), "lines.jsonl")
	wantDetails := [][2]bool{2: {false, false}, 3: {false, true}, 4: {true, true}, 5: {true, true}}
	for n, line := range batchLines {
		i := n + 1 // records appended
		appendFile(t, path, line+"\n")
		snap, err := loadTranscript("codex", "s", path)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			continue
		}
		tools := snap.items[len(snap.items)-1].Tools
		if len(snap.items) != 2 || len(tools) != 2 || !strings.Contains(tools[0].Label, "pwd") || !strings.Contains(tools[1].Label, "false") {
			t.Fatalf("after %d records: %+v", i, snap.items)
		}
		if d := [2]bool{tools[0].Detail != "", tools[1].Detail != ""}; d != wantDetails[i] {
			t.Fatalf("after %d records: details %v, want %v", i, d, wantDetails[i])
		}
	}
	rng := rand.New(rand.NewSource(2))
	for round := 0; round < 20; round++ {
		path := filepath.Join(t.TempDir(), "grow.jsonl")
		var snaps []transcriptSnapshot
		var copies [][]Item
		for pos := 0; pos < len(data); {
			n := min(1+rng.Intn(256), len(data)-pos)
			appendFile(t, path, data[pos:pos+n])
			pos += n
			snap, err := loadTranscript("codex", "s", path)
			if err != nil {
				t.Fatal(err)
			}
			snaps, copies = append(snaps, snap), append(copies, cloneItems(snap.items))
		}
		if last := snaps[len(snaps)-1]; !reflect.DeepEqual(last.items, want.items) {
			t.Fatalf("round %d: incremental index differs from full read:\n%+v\n%+v", round, last.items, want.items)
		}
		for i := range snaps {
			if !reflect.DeepEqual(snaps[i].items, copies[i]) {
				t.Fatalf("round %d: published snapshot %d was mutated", round, i)
			}
		}
	}
}

func cloneItems(items []Item) []Item {
	out := slices.Clone(items)
	for i := range out {
		out[i].Tools = slices.Clone(out[i].Tools)
	}
	return out
}
