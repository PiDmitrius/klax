package main

import (
	"context"

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
	session int64  // the tab (session Created) this turn belongs to
}

func (d *daemon) newUIDelivery(_ context.Context, msg queuedMsg) *uiDelivery {
	u := &uiDelivery{d: d, user: userFromKey(msg.chatID), session: msg.sessCreated}
	d.uiEmit(u.user, uiEvent{Type: "turn_start", Session: u.session})
	return u
}

func (u *uiDelivery) Progress(ev runner.ProgressEvent) {
	// The runner pre-formats ev.Text at the narrow Telegram width. The UI has
	// the room, so for real tool calls re-render the structured tool at the
	// wider UI limit; other events (narration, rate-limit, errors) pass through.
	text := ev.Text
	if ev.Tool != nil {
		text = ev.Tool.Preview(runner.UIToolPreviewLimit)
	}
	u.d.uiEmit(u.user, uiEvent{
		Type:    "progress",
		Session: u.session,
		Kind:    string(ev.Kind),
		Text:    text,
	})
}

func (u *uiDelivery) Final(res runner.RunResult) {
	if res.Error != nil {
		u.d.uiEmit(u.user, uiEvent{Type: "error", Session: u.session, Text: res.Error.Error()})
		return
	}
	u.d.uiEmit(u.user, uiEvent{
		Type:      "final",
		Session:   u.session,
		Markdown:  res.Text,
		Model:     res.Usage.Model,
		CtxUsed:   res.Usage.ContextUsed,
		CtxWindow: res.Usage.ContextWindow,
	})
}

func (u *uiDelivery) Close() {}
