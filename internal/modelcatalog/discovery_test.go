package modelcatalog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCodexPagesAndNotifications(t *testing.T) {
	input := `{"id":1,"result":{}}
{"method":"notification","params":{}}
{"id":2,"result":{"data":[{"model":"gpt-a","displayName":"A","isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"high"}]},{"model":"hidden","displayName":"Hidden","hidden":true}],"nextCursor":"page2"}}
{"id":3,"result":{"data":[{"model":"gpt-b","displayName":"B"}],"nextCursor":null}}
`
	var sent bytes.Buffer
	p := protocol{json.NewEncoder(&sent), bufio.NewScanner(strings.NewReader(input))}
	got, err := p.codex()
	if err != nil || !reflect.DeepEqual(got, []Model{{Value: "gpt-a", Label: "gpt-a", Default: true, ThinkLevels: []string{"low", "high"}}, {Value: "gpt-b", Label: "gpt-b"}}) {
		t.Fatal(got, err)
	}
	dec := json.NewDecoder(&sent)
	for i, method := range []string{"initialize", "initialized", "model/list", "model/list"} {
		var req struct {
			Method string
			Params struct{ Cursor string }
		}
		if err := dec.Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Method != method {
			t.Fatal(req)
		}
		if i == 3 && req.Params.Cursor != "page2" {
			t.Fatal("cursor not forwarded")
		}
	}
}

func TestClaudeUsesResolvedIDsAndDeduplicates(t *testing.T) {
	input := `{"type":"system","subtype":"init"}
{"type":"control_response","response":{"request_id":"other","subtype":"success"}}
{"type":"control_response","response":{"request_id":"models","subtype":"success","response":{"models":[{"value":"default","displayName":"Default","resolvedModel":"claude-opus-example[1m]","supportsEffort":true,"supportedEffortLevels":["high","max"]},{"value":"opus[1m]","displayName":"Opus","resolvedModel":"claude-opus-example[1m]"},{"value":"claude-example-1[1m]","displayName":"Example","resolvedModel":"claude-example-1"}]}}}
`
	var sent bytes.Buffer
	p := protocol{json.NewEncoder(&sent), bufio.NewScanner(strings.NewReader(input))}
	got, err := p.claude()
	if err != nil || !reflect.DeepEqual(got, []Model{{Value: "claude-opus-example[1m]", Label: "claude-opus-example[1m]", Default: true, ThinkLevels: []string{"high", "max"}}, {Value: "claude-example-1", Label: "claude-example-1"}}) {
		t.Fatal(got, err)
	}
	var req struct {
		Type    string
		Request struct{ Subtype string }
	}
	if err := json.Unmarshal(sent.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	if req.Type != "control_request" || req.Request.Subtype != "initialize" {
		t.Fatal(req)
	}
}

func TestInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		claude      bool
	}{
		{"invalid JSON", "not JSON", false},
		{"missing result", `{"id":1}`, false},
		{"error", `{"id":1,"error":{"code":-1,"message":"denied"}}`, false},
		{"missing list", "{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":{}}", false},
		{"EOF", "{\"id\":1,\"result\":{}}\n", false},
		{"unresolved Claude alias", `{"type":"control_response","response":{"request_id":"models","subtype":"success","response":{"models":[{"value":"opus","displayName":"Opus"}]}}}`, true},
		{"claude error", `{"type":"control_response","response":{"request_id":"models","subtype":"error","error":"denied"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := protocol{json.NewEncoder(&bytes.Buffer{}), bufio.NewScanner(strings.NewReader(tc.input))}
			var err error
			if tc.claude {
				_, err = p.claude()
			} else {
				_, err = p.codex()
			}
			if err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestFetchCancellationReapsProcess(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	// exec avoids an extra child; the blocked read must be interrupted by context cancellation.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Fetch(ctx, "claude")
	if err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("process was not stopped promptly")
	}
}

func TestFetchInstalledCLIs(t *testing.T) {
	if os.Getenv("KLAX_TEST_MODEL_DISCOVERY") != "1" {
		t.Skip("explicit local CLI check")
	}
	for _, backend := range []string{"codex", "claude"} {
		t.Run(backend, func(t *testing.T) {
			start := time.Now()
			models, err := Fetch(context.Background(), backend)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %d models in %s", backend, len(models), time.Since(start))
		})
	}
}
