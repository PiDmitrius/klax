// Package turnaudit defines klax's stable per-turn audit protocol and invokes
// its local synchronous executable sink.
package turnaudit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/history"
	"github.com/PiDmitrius/klax/internal/inbound"
)

const (
	Schema       = "klax.audit/v1"
	turnIDDomain = "klax.turn/v1"
)

type Attachment struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Routing struct {
	SessionKey     string `json:"session_key"`
	SessionCreated int64  `json:"session_created"`
	SessionName    string `json:"session_name,omitempty"`
}

type Request struct {
	OriginalText    string       `json:"original_text"`
	EffectivePrompt string       `json:"effective_prompt"`
	Attachments     []Attachment `json:"attachments,omitempty"`
}

type Execution struct {
	Backend            string `json:"backend"`
	BackendSessionID   string `json:"backend_session_id,omitempty"`
	CWD                string `json:"cwd"`
	ModelRequested     string `json:"model_requested,omitempty"`
	Effort             string `json:"effort,omitempty"`
	Sandbox            string `json:"sandbox,omitempty"`
	TTY                bool   `json:"tty"`
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"`
}

type Tokens struct {
	Input         int `json:"input,omitempty"`
	Output        int `json:"output,omitempty"`
	CacheRead     int `json:"cache_read,omitempty"`
	CacheCreation int `json:"cache_creation,omitempty"`
}

type ContextAfter struct {
	Used   int `json:"used,omitempty"`
	Window int `json:"window,omitempty"`
}

type Output struct {
	Text   string `json:"text"`
	Format string `json:"format"`
}

type AuditError struct {
	Stage   string `json:"stage"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Elapsed struct {
	Queued    int64 `json:"queued"`
	StartHook int64 `json:"start_hook"`
	Backend   int64 `json:"backend"`
	Finalize  int64 `json:"finalize"`
	Total     int64 `json:"total"`
}

type Result struct {
	Status       string       `json:"status"`
	Output       *Output      `json:"output,omitempty"`
	Error        *AuditError  `json:"error,omitempty"`
	ModelUsed    string       `json:"model_used,omitempty"`
	Tokens       Tokens       `json:"tokens"`
	ContextAfter ContextAfter `json:"context_after"`
	ElapsedMS    Elapsed      `json:"elapsed_ms"`
}

type RawTrace struct {
	Path      string `json:"path"`
	FromEvent int64  `json:"from_event"`
	ToEvent   int64  `json:"to_event"`
	SHA256    string `json:"sha256"`
}

type Trace struct {
	Blocks []history.Item `json:"blocks"`
	Raw    RawTrace       `json:"raw"`
}

type Turn struct {
	ID         string         `json:"turn_id"`
	Seq        int64          `json:"turn_seq"`
	AcceptedAt string         `json:"accepted_at"`
	StartAt    string         `json:"start_at"`
	Origin     inbound.Origin `json:"origin"`
	Routing    Routing        `json:"routing"`
	Request    Request        `json:"request"`
	Execution  Execution      `json:"execution"`
	FinishAt   string         `json:"finish_at,omitempty"`
	Result     *Result        `json:"result,omitempty"`
	Trace      *Trace         `json:"trace,omitempty"`
}

type Event struct {
	Schema string `json:"schema"`
	Event  string `json:"event"`
	Turn   Turn   `json:"turn"`
}

// TurnID is the lowercase hex form of the first 160 bits of SHA-256 over the
// canonical durable identity of a turn. The JSON array is the versioned,
// unambiguous byte contract; changing it would change public IDs.
func TurnID(sessionKey string, sessionCreated, seq int64) string {
	raw, _ := json.Marshal([]any{turnIDDomain, sessionKey, sessionCreated, seq})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:20])
}

func Time(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func Millis(d time.Duration) int64 { return d.Milliseconds() }

func UnixSeconds(v int64) string {
	if v == 0 {
		return ""
	}
	return Time(time.Unix(v, 0))
}

func UnixMillis(v int64) string {
	if v == 0 {
		return ""
	}
	return Time(time.UnixMilli(v))
}

func UnixID(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

var ErrNotConfigured = errors.New("audit hook not configured")

// Invoke executes the configured command synchronously with one JSON object and
// trailing newline on stdin. Stdout is deliberately outside the protocol.
func Invoke(parent context.Context, cfg *config.AuditHookConfig, event Event) error {
	if cfg == nil || len(cfg.Command) == 0 || cfg.Command[0] == "" {
		return ErrNotConfigured
	}
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal audit event: %w", err)
	}
	body = append(body, '\n')
	cmd := exec.CommandContext(parent, cfg.Command[0], cfg.Command[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{dst: &stderr, remaining: 64 << 10}
	if err := cmd.Run(); err != nil {
		if parent.Err() != nil {
			return fmt.Errorf("audit hook: %w", parent.Err())
		}
		if stderr.Len() > 0 {
			return fmt.Errorf("audit hook: %w: %s", err, stderr.String())
		}
		return fmt.Errorf("audit hook: %w", err)
	}
	return nil
}

type limitedWriter struct {
	dst       *bytes.Buffer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	_, _ = w.dst.Write(p)
	w.remaining -= len(p)
	return n, nil
}
