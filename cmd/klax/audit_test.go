package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/inbound"
	"github.com/PiDmitrius/klax/internal/runner"
	"github.com/PiDmitrius/klax/internal/sessfiles"
	"github.com/PiDmitrius/klax/internal/session"
	"github.com/PiDmitrius/klax/internal/turnaudit"
)

func TestAuditEventsShareDeterministicTurnID(t *testing.T) {
	accepted := time.Date(2026, 7, 25, 9, 34, 50, 0, time.UTC)
	started := accepted.Add(6 * time.Second)
	msg := queuedMsg{
		chatID: "ym:0/0/group#7", msgID: "99",
		text: "effective", originalText: "@bot effective",
		turnSeq: 42, sessKey: "user:ivan", sessCreated: 8,
		acceptedAt: accepted.UnixNano(),
		origin: inbound.Origin{
			Transport: "ym",
			Chat:      inbound.Chat{ID: "0/0/group", Type: "group", ThreadID: "7"},
			Message:   inbound.Message{ID: "99"},
			Sender:    inbound.Sender{ID: "u1", Username: "ivan@example.org"},
		},
	}
	turn, err := newAuditTurn(msg, nil, &session.Session{Name: "work", CWD: "/work"}, "effective", "codex", started)
	if err != nil {
		t.Fatal(err)
	}
	begin := startAuditEvent(turn)
	end := finishAuditEvent(turn, runner.RunResult{
		Text: "done", Usage: runner.ModelUsageInfo{Model: "gpt", InputTokens: 3, OutputTokens: 1},
	}, nil, 4, 100, started, started, started.Add(2*time.Second), started.Add(2*time.Second))

	if begin.Turn.ID == "" || begin.Turn.ID != end.Turn.ID {
		t.Fatalf("turn ids differ: started=%q finished=%q", begin.Turn.ID, end.Turn.ID)
	}
	if begin.Turn.Result != nil || begin.Turn.FinishAt != "" {
		t.Fatalf("turn.start contains terminal fields: %+v", begin.Turn)
	}
	if end.Turn.Result == nil || end.Turn.Result.Output == nil || end.Turn.Result.Output.Text != "done" {
		t.Fatalf("finished result = %+v", end.Turn.Result)
	}
	if end.Turn.Request.OriginalText != "@bot effective" || end.Turn.Request.EffectivePrompt != "effective" {
		t.Fatalf("request = %+v", end.Turn.Request)
	}
	if elapsed := end.Turn.Result.ElapsedMS; elapsed.Queued != 6000 || elapsed.Backend != 2000 || elapsed.Total != 8000 {
		t.Fatalf("elapsed = %+v", elapsed)
	}
}

func TestFinishedAuditClassifiesAbort(t *testing.T) {
	now := time.Now()
	turn, err := newAuditTurn(queuedMsg{turnSeq: 1, sessKey: "group", sessCreated: 1}, nil, &session.Session{CWD: "/work"}, "go", "codex", now)
	if err != nil {
		t.Fatal(err)
	}
	event := finishAuditEvent(turn, runner.RunResult{Error: context.Canceled}, nil, 0, 0, now, now, now, now)
	if event.Turn.Result.Status != "aborted" || event.Turn.Result.Error.Code != "aborted" {
		t.Fatalf("abort result = %+v", event.Turn.Result)
	}
}

func TestAuditJSONUsesDocumentedV1Names(t *testing.T) {
	now := time.Now()
	turn, err := newAuditTurn(
		queuedMsg{turnSeq: 1, sessKey: "user:test", sessCreated: 1},
		nil,
		&session.Session{CWD: "/work", ModelOverride: "requested"},
		"go", "codex", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	event := finishAuditEvent(turn, runner.RunResult{
		Usage: runner.ModelUsageInfo{Model: "used", InputTokens: 3},
	}, nil, 4, 100, now, now, now, now)
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{`"model_requested"`, `"model_used"`, `"tokens"`, `"context_after"`, `"elapsed_ms"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("event JSON lacks %s: %s", want, text)
		}
	}
	for _, obsolete := range []string{`"occurred_at"`, `"canonical_user"`, `"duration_ms"`, `"usage"`} {
		if strings.Contains(text, obsolete) {
			t.Fatalf("event JSON contains obsolete %s: %s", obsolete, text)
		}
	}
}

func TestAuditDisabledUnlessACommandIsConfigured(t *testing.T) {
	for _, cfg := range []*config.Config{
		nil,
		{},
		{Audit: &config.AuditConfig{}},
		{Audit: &config.AuditConfig{Turn: &config.AuditTurnConfig{}}},
		{Audit: &config.AuditConfig{Turn: &config.AuditTurnConfig{Start: &config.AuditHookConfig{}}}},
	} {
		if (&daemon{cfg: cfg}).auditEnabled() {
			t.Fatalf("audit enabled for %#v", cfg)
		}
	}
	if !(&daemon{cfg: &config.Config{Audit: &config.AuditConfig{Turn: &config.AuditTurnConfig{
		Finish: &config.AuditHookConfig{Command: []string{"finish"}},
	},
	}}}).auditEnabled() {
		t.Fatal("configured turn.finish hook did not enable audit snapshots")
	}
}

func TestAuditHooksAreSelectedIndependently(t *testing.T) {
	start := &config.AuditHookConfig{Command: []string{"start"}}
	finish := &config.AuditHookConfig{Command: []string{"finish"}}
	d := &daemon{cfg: &config.Config{Audit: &config.AuditConfig{Turn: &config.AuditTurnConfig{
		Start: start, Finish: finish,
	},
	}}}
	if got := d.auditHook("turn.start"); got != start {
		t.Fatalf("turn.start hook = %#v", got)
	}
	if got := d.auditHook("turn.finish"); got != finish {
		t.Fatalf("turn.finish hook = %#v", got)
	}
	if got := d.auditHook("unknown"); got != nil {
		t.Fatalf("unknown hook = %#v, want nil", got)
	}
}

func TestInvokeAuditReturnsConfiguredHookFailure(t *testing.T) {
	d := &daemon{cfg: &config.Config{Audit: &config.AuditConfig{Turn: &config.AuditTurnConfig{
		Start: &config.AuditHookConfig{Command: []string{"/bin/false"}},
	}}}}
	err := d.invokeAudit(startAuditEvent(turnaudit.Turn{ID: strings.Repeat("0", 40)}))
	if err == nil {
		t.Fatal("failed start hook was treated as success")
	}
}

func TestAuditUsesPublicMaxNameAndDurableAttachmentPath(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	store := sessfiles.Open("mx:chat", 2)
	stored, err := store.WriteFile(1, 1, "report.txt", strings.NewReader("report"))
	if err != nil {
		t.Fatal(err)
	}
	turn, err := newAuditTurn(queuedMsg{
		chatID: "mx:chat", files: []string{stored},
		turnSeq: 1, sessKey: "mx:chat", sessCreated: 2,
		origin: inbound.Origin{Transport: "mx", Chat: inbound.Chat{ID: "chat"}},
	}, store, &session.Session{CWD: "/work"}, "inspect", "claude", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if turn.Origin.Transport != "max" {
		t.Fatalf("audit transport = %q, want max", turn.Origin.Transport)
	}
	if len(turn.Request.Attachments) != 1 {
		t.Fatalf("attachments = %+v", turn.Request.Attachments)
	}
	got := turn.Request.Attachments[0]
	if got.Name != "report.txt" || got.Path != store.Path(stored) || got.Size != 6 {
		t.Fatalf("attachment = %+v", got)
	}
}
