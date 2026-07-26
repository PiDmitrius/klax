package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/history"
	"github.com/PiDmitrius/klax/internal/inbound"
	"github.com/PiDmitrius/klax/internal/runner"
	"github.com/PiDmitrius/klax/internal/sessfiles"
	"github.com/PiDmitrius/klax/internal/session"
	"github.com/PiDmitrius/klax/internal/turnaudit"
)

func (d *daemon) auditHook(phase string) *config.AuditHookConfig {
	if d.cfg == nil || d.cfg.Audit == nil || d.cfg.Audit.Turn == nil {
		return nil
	}
	switch phase {
	case "turn.start":
		return d.cfg.Audit.Turn.Start
	case "turn.finish":
		return d.cfg.Audit.Turn.Finish
	default:
		return nil
	}
}

func (d *daemon) auditEnabled() bool {
	for _, phase := range []string{"turn.start", "turn.finish"} {
		hook := d.auditHook(phase)
		if hook != nil && len(hook.Command) > 0 && hook.Command[0] != "" {
			return true
		}
	}
	return false
}

func (d *daemon) invokeAudit(event turnaudit.Event) error {
	hook := d.auditHook(event.Event)
	if hook == nil || len(hook.Command) == 0 || hook.Command[0] == "" {
		return nil
	}
	if err := turnaudit.Invoke(context.Background(), hook, event); err != nil {
		log.Printf("audit %s (%s): %v", event.Event, event.Turn.ID, err)
		return err
	}
	return nil
}

func auditOrigin(origin inbound.Origin, chatID, msgID string) inbound.Origin {
	if origin.Transport == "" {
		origin.Transport = transportPrefix(chatID)
	}
	if origin.Chat.ID == "" {
		origin.Chat.ID = strings.TrimPrefix(chatID, transportPrefix(chatID)+":")
	}
	if origin.Message.ID == "" {
		origin.Message.ID = msgID
	}
	if origin.Transport == "mx" {
		origin.Transport = "max"
	}
	return origin
}

func auditAttachments(store *sessfiles.Store, stored []string) ([]turnaudit.Attachment, error) {
	if len(stored) == 0 {
		return nil, nil
	}
	names := sessfiles.RunNames(stored)
	out := make([]turnaudit.Attachment, 0, len(stored))
	for i, storedName := range stored {
		path := store.Path(storedName)
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		size, err := io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, turnaudit.Attachment{
			Name: names[i], Path: path, Size: size, SHA256: hex.EncodeToString(h.Sum(nil)),
		})
	}
	return out, nil
}

func newAuditTurn(msg queuedMsg, store *sessfiles.Store, sess *session.Session, prompt, backend string, started time.Time) (turnaudit.Turn, error) {
	accepted := time.Unix(0, msg.acceptedAt)
	if msg.acceptedAt == 0 {
		accepted = started
	}
	attachments, err := auditAttachments(store, msg.files)
	if err != nil {
		return turnaudit.Turn{}, fmt.Errorf("snapshot audit attachments: %w", err)
	}
	return turnaudit.Turn{
		ID:         turnaudit.TurnID(msg.sessKey, msg.sessCreated, msg.turnSeq),
		Seq:        msg.turnSeq,
		AcceptedAt: turnaudit.Time(accepted),
		StartAt:    turnaudit.Time(started),
		Origin:     auditOrigin(msg.origin, msg.chatID, msg.msgID),
		Routing: turnaudit.Routing{
			SessionKey: msg.sessKey, SessionCreated: msg.sessCreated, SessionName: sess.Name,
		},
		Request: turnaudit.Request{
			OriginalText: msg.originalText, EffectivePrompt: prompt, Attachments: attachments,
		},
		Execution: turnaudit.Execution{
			Backend: backend, BackendSessionID: sess.ID, CWD: sess.CWD,
			ModelRequested: sess.ModelOverride, Effort: sess.ThinkOverride, Sandbox: sess.Sandbox,
			TTY: sess.ClaudeTTY, AppendSystemPrompt: sess.AppendSystemPrompt,
		},
	}, nil
}

func startAuditEvent(turn turnaudit.Turn) turnaudit.Event {
	return turnaudit.Event{Schema: turnaudit.Schema, Event: "turn.start", Turn: turn}
}

func auditTrace(store *sessfiles.Store, seq int64, backend, sessionID string, session *history.AuditSession) (*turnaudit.Trace, error) {
	turns, err := store.InboundLog()
	if err != nil {
		return nil, err
	}
	var bound *sessfiles.Turn
	for i := range turns {
		if turns[i].Seq == seq {
			bound = &turns[i]
			break
		}
	}
	if bound == nil || !bound.Bound || bound.Backend != backend || bound.Session != sessionID {
		return nil, errors.New("turn has no matching durable backend user-event binding")
	}
	snap, err := session.Turn(bound.Event)
	if err != nil {
		return nil, err
	}
	return &turnaudit.Trace{
		Blocks: snap.Blocks,
		Raw: turnaudit.RawTrace{
			Path: snap.Path, FromEvent: snap.FromEvent,
			ToEvent: snap.ToEvent, SHA256: snap.SHA256,
		},
	}, nil
}

func finishAuditEvent(turn turnaudit.Turn, res runner.RunResult, trace *turnaudit.Trace, ctxUsed, ctxWindow int, started, backendStarted, backendFinished, finished time.Time) turnaudit.Event {
	status := "success"
	var auditErr *turnaudit.AuditError
	if res.Error != nil {
		status = "error"
		stage, code := "backend", turnErrorReason(res.Error)
		if errors.Is(res.Error, context.Canceled) {
			status, stage, code = "aborted", "klax", "aborted"
		} else if code == turnErrRunStartFailed || code == turnErrAttachmentsMissing {
			stage = "klax"
		}
		auditErr = &turnaudit.AuditError{Stage: stage, Code: code, Message: res.Error.Error()}
	}
	var output *turnaudit.Output
	if res.Error == nil || res.Text != "" {
		output = &turnaudit.Output{Text: res.Text, Format: "markdown"}
	}
	turn.FinishAt = turnaudit.Time(finished)
	turn.Trace = trace
	turn.Result = &turnaudit.Result{
		Status: status, Output: output, Error: auditErr,
		ModelUsed: res.Usage.Model,
		Tokens: turnaudit.Tokens{
			Input: res.Usage.InputTokens, Output: res.Usage.OutputTokens,
			CacheRead: res.Usage.CacheRead, CacheCreation: res.Usage.CacheCreation,
		},
		ContextAfter: turnaudit.ContextAfter{Used: ctxUsed, Window: ctxWindow},
		ElapsedMS: turnaudit.Elapsed{
			Queued:    nonNegativeMillis(started.Sub(time.Unix(0, msgAcceptedAt(turn, started)))),
			StartHook: nonNegativeMillis(backendStarted.Sub(started)),
			Backend:   nonNegativeMillis(backendFinished.Sub(backendStarted)),
			Finalize:  nonNegativeMillis(finished.Sub(backendFinished)),
		},
	}
	elapsed := &turn.Result.ElapsedMS
	elapsed.Total = elapsed.Queued + elapsed.StartHook + elapsed.Backend + elapsed.Finalize
	return turnaudit.Event{Schema: turnaudit.Schema, Event: "turn.finish", Turn: turn}
}

func nonNegativeMillis(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return turnaudit.Millis(d)
}

func msgAcceptedAt(turn turnaudit.Turn, fallback time.Time) int64 {
	t, err := time.Parse(time.RFC3339Nano, turn.AcceptedAt)
	if err != nil {
		return fallback.UnixNano()
	}
	return t.UnixNano()
}
