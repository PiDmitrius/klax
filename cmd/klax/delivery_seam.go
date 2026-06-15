package main

import (
	"context"

	"github.com/PiDmitrius/klax/internal/runner"
)

// Delivery streams a single turn's progress and final result to one channel
// (a messenger chat, the web UI, ...). It is created per turn by deliveryFor,
// AFTER the session is chosen, and owns ALL channel-specific output shaping
// (splitting, formatting, edit-streaming for messengers; JSON events for the
// UI). It deliberately does NOT persist session state: the caller (runBackend)
// updates the store between the run and Final, so business logic stays in one
// place and never gets duplicated across delivery backends.
type Delivery interface {
	// Progress receives one streamed event. It runs in the runner's
	// stdout-scanner goroutine under narrationBuffer.mu, so it MUST NOT block
	// on the network — hand work to a worker/mailbox and return immediately.
	Progress(ev runner.ProgressEvent)
	// Final delivers the completed turn (answer or error). It first stops the
	// progress stream (barrier), then renders and sends the result. Called once
	// per turn, after the caller has persisted the session record.
	Final(res runner.RunResult)
	// Close releases resources. Idempotent and safe to call even after Final —
	// runBackend defers it so an early return still tears the delivery down.
	Close()
}

// deliveryFor builds the Delivery for a turn, picked by the chat's transport
// prefix. Messenger chats (tg/mx/vk and anything else) get the edit-streaming
// messengerDelivery; the "ui" prefix is routed to the web-UI delivery (added
// alongside the UI server).
func (d *daemon) deliveryFor(ctx context.Context, msg queuedMsg, verbose bool) Delivery {
	if transportPrefix(msg.chatID) == uiPrefix {
		return d.newUIDelivery(ctx, msg)
	}
	return d.newMessengerDelivery(ctx, msg, verbose)
}
