package history

import (
	"strings"
	"testing"
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
			got := decodeCodexExecTools(tc.input)
			if tc.wantName == "" {
				if got != nil {
					t.Fatalf("want nil (fallback), got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want 1 tool, got %d: %+v", len(got), got)
			}
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
