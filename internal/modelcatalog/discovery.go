package modelcatalog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/PiDmitrius/klax/internal/runner"
)

type Model struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Fetch reads CLI control messages only; it never submits a user turn.
func Fetch(ctx context.Context, backend string) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var args []string
	switch backend {
	case "codex":
		args = []string{"app-server"}
	case "claude":
		args = []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--no-session-persistence", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--setting-sources", "user"}
	default:
		return nil, errors.New("unknown backend")
	}
	bin := runner.FindBinary(backend)
	if bin == "" {
		return nil, fmt.Errorf("%s CLI not found", backend)
	}
	dir, err := os.MkdirTemp("", "klax-models-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { _ = cmd.Cancel(); _ = in.Close(); _ = cmd.Wait() }()
	stopRead := context.AfterFunc(ctx, func() { _ = out.Close() })
	defer stopRead()
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 4096), 4<<20)
	p := protocol{in: json.NewEncoder(in), out: scan}
	var models []Model
	if backend == "codex" {
		models, err = p.codex()
	} else {
		models, err = p.claude()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", backend, err)
	}
	if err = validate(models); err != nil {
		return nil, fmt.Errorf("%s: %w", backend, err)
	}
	return models, nil
}

type protocol struct {
	in  *json.Encoder
	out *bufio.Scanner
}

func (p *protocol) read(dst any) error {
	if !p.out.Scan() {
		if err := p.out.Err(); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	if err := json.Unmarshal(p.out.Bytes(), dst); err != nil {
		return errors.New("invalid JSON control response")
	}
	return nil
}

func (p *protocol) call(id int, method string, params any, dst any) error {
	if err := p.in.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		var msg struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := p.read(&msg); err != nil {
			return err
		}
		if msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return fmt.Errorf("%s: %s (%d)", method, msg.Error.Message, msg.Error.Code)
		}
		if len(msg.Result) == 0 || string(msg.Result) == "null" {
			return errors.New("missing control result")
		}
		return json.Unmarshal(msg.Result, dst)
	}
}

func (p *protocol) codex() ([]Model, error) {
	var init json.RawMessage
	if err := p.call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "klax", "version": "1"}}, &init); err != nil {
		return nil, err
	}
	if err := p.in.Encode(map[string]string{"method": "initialized"}); err != nil {
		return nil, err
	}
	var models []Model
	cursor := ""
	seen := map[string]bool{}
	for id := 2; ; id++ {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result struct {
			Data []struct {
				Model       string `json:"model"`
				DisplayName string `json:"displayName"`
				Hidden      bool   `json:"hidden"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := p.call(id, "model/list", params, &result); err != nil {
			return nil, err
		}
		if result.Data == nil {
			return nil, errors.New("missing model list")
		}
		for _, m := range result.Data {
			if !m.Hidden {
				models = append(models, Model{m.Model, m.DisplayName})
			}
		}
		cursor = result.NextCursor
		if cursor == "" {
			return models, nil
		}
		if seen[cursor] {
			return nil, errors.New("repeated model page cursor")
		}
		seen[cursor] = true
	}
}

func (p *protocol) claude() ([]Model, error) {
	if err := p.in.Encode(map[string]any{"type": "control_request", "request_id": "models", "request": map[string]string{"subtype": "initialize"}}); err != nil {
		return nil, err
	}
	for {
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				RequestID string `json:"request_id"`
				Subtype   string `json:"subtype"`
				Error     string `json:"error"`
				Response  struct {
					Models []struct {
						Value       string `json:"value"`
						DisplayName string `json:"displayName"`
					} `json:"models"`
				} `json:"response"`
			} `json:"response"`
		}
		if err := p.read(&msg); err != nil {
			return nil, err
		}
		if msg.Type != "control_response" || msg.Response.RequestID != "models" {
			continue
		}
		if msg.Response.Subtype != "success" {
			return nil, fmt.Errorf("initialize: %s", msg.Response.Error)
		}
		var models []Model
		for _, m := range msg.Response.Response.Models {
			if m.Value != "default" {
				models = append(models, Model{m.Value, m.DisplayName})
			}
		}
		return models, nil
	}
}

func validate(models []Model) error {
	if len(models) == 0 {
		return errors.New("empty model catalog")
	}
	seen := map[string]bool{}
	for _, m := range models {
		if m.Value == "" || m.Label == "" || seen[m.Value] {
			return errors.New("invalid or duplicate model entry")
		}
		seen[m.Value] = true
	}
	return nil
}
