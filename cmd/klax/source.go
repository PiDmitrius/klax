package main

import "context"

// Inbound is a normalized incoming message from any channel (tg/mx/vk/ui). A
// Source produces it after its platform-specific gating (allow-list, /id,
// attachment download); the daemon then routes every Inbound uniformly through
// handleInbound, so command/group/enqueue logic lives in exactly one place.
type Inbound struct {
	ChatID      string
	MsgID       string // user's message ID (for replyTo)
	Text        string
	Attachments []attachment
	// TargetCreated binds the message to a specific session. 0 means "the
	// active session" — every messenger uses this. The web UI sets it to a
	// tab's Created so a message lands in that tab even when it is not active.
	TargetCreated int64
	FromID        int64 // sender ID (platform-scoped), for diagnostics
	// RawMessage suppresses "/"-command dispatch: the text is always queued as a
	// message, never parsed as a slash command. The web UI sets this — it has no
	// chat commands (every action is a native control), so a message that starts
	// with "/" is just text for the agent.
	RawMessage bool
	// Nonce is the web UI's per-send id, echoed back in the "user" event so the
	// sending tab skips its own optimistic echo while other tabs render it. Empty
	// for messenger sources (every UI tab then renders their message live).
	Nonce string
}

// Source is an inbound channel into the daemon: a loop (poll or server) that
// feeds messages in via handleInbound. tg/mx/vk are poll-loop sources; the web
// UI is an HTTP (long-poll) source. Run blocks until ctx is cancelled.
type Source interface {
	Name() string
	Run(ctx context.Context)
}

// legacySource adapts an existing poll loop (pollTG/pollMAX/pollVK) to the
// Source interface without disturbing its battle-tested body: the allow-list,
// /id replies and attachment download stay inside the loop, which still calls
// handleMessageWithAttachments (and thus handleInbound) for allowed messages.
type legacySource struct {
	name string
	poll func(context.Context)
}

func (s *legacySource) Name() string            { return s.name }
func (s *legacySource) Run(ctx context.Context) { s.poll(ctx) }
