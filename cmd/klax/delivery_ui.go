package main

import (
	"context"
	"time"

	"github.com/PiDmitrius/klax/internal/runner"
)

// uiDelivery streams a turn to the web UI as JSON events over SSE. Unlike the
// messenger delivery it does no splitting, formatting or edit-streaming: each
// event is forwarded verbatim (tagged with the tab's session) and the client
// renders Markdown itself. Emits are non-blocking (the hub drops a slow client
// rather than stalling the runner's stdout goroutine).
type uiDelivery struct {
	d       *daemon
	user    string // canonical user (hub key)
	sk      string // session key (for outbound file rewrite)
	session int64  // the tab (session Created) this turn belongs to
	turnSeq int64  // the durable turn id, stamped on every turn-scoped event
}

func (d *daemon) newUIDelivery(_ context.Context, msg queuedMsg) *uiDelivery {
	u := &uiDelivery{d: d, user: uiUserForKey(msg.sessKey), sk: msg.sessKey, session: msg.sessCreated, turnSeq: msg.turnSeq}
	d.uiEmit(u.user, uiEvent{Type: "turn_start", Session: u.session, TurnSeq: u.turnSeq, State: "run"})
	return u
}

func (u *uiDelivery) Progress(ev runner.ProgressEvent) {
	if ev.Kind == runner.ProgressKindContext {
		u.d.uiEmit(u.user, uiEvent{
			Type: "context", Session: u.session, TurnSeq: u.turnSeq,
			CtxUsed: ev.Usage.ContextUsed, CtxWindow: ev.Usage.ContextWindow,
		})
		return
	}
	// The runner pre-formats ev.Text at the narrow Telegram width. The UI has the room,
	// so a real tool call is re-rendered at the wider UI limit; narration passes through.
	// Each block gets a stable id so the client dedups a reload-race duplicate.
	now := time.Now().Format(time.RFC3339)
	var b uiBlock
	if ev.Tool != nil {
		p := ev.Tool.Preview(runner.UIToolPreviewLimit)
		b = uiBlock{ID: blockID(u.turnSeq, "tool", p, nil), Role: "tool", Text: p, Time: now}
	} else {
		b = uiBlock{ID: blockID(u.turnSeq, "assistant", ev.Text, nil), Role: "assistant", Text: ev.Text, Time: now}
	}
	u.d.uiEmit(u.user, uiEvent{
		Type: "progress", Session: u.session, TurnSeq: u.turnSeq, Block: &b, Kind: string(ev.Kind),
		CtxUsed: ev.Usage.ContextUsed, CtxWindow: ev.Usage.ContextWindow,
	})
}

func (u *uiDelivery) Final(res runner.RunResult) {
	if res.Error != nil {
		msg := turnErrorReason(res.Error)
		b := errBlock(u.turnSeq, msg)
		b.Time = time.Now().Format(time.RFC3339)
		u.d.uiEmit(u.user, uiEvent{Type: "error", Session: u.session, TurnSeq: u.turnSeq, State: "err", Block: &b, Text: b.Text})
		return
	}
	// Block id is hashed from the CANONICAL answer (res.Text) BEFORE the outbound file-ref
	// rewrite, so the live final and the same answer re-read from the transcript share one
	// id and the client dedups the reload-race duplicate (REFACTOR_PLAN §A3).
	id := blockID(u.turnSeq, "assistant", res.Text, nil)
	md := u.d.rewriteOutboundForUI(u.sk, u.session, res.Text)
	b := uiBlock{ID: id, Role: "assistant", Text: md, Time: time.Now().Format(time.RFC3339)}
	u.d.uiEmit(u.user, uiEvent{
		Type: "final", Session: u.session, TurnSeq: u.turnSeq, State: "done", Block: &b,
		Markdown: md, Model: res.Usage.Model, CtxUsed: res.Usage.ContextUsed, CtxWindow: res.Usage.ContextWindow,
	})
}

func (u *uiDelivery) Close() {}
