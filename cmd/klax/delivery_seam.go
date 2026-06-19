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

// teeDelivery forwards one turn's stream to two deliveries (a messenger chat AND
// the web-UI mirror). Both Progress/Final/Close run in order; each is itself
// non-blocking (messenger hands to a worker, UI emits to the per-user ring).
type teeDelivery struct{ a, b Delivery }

func (t teeDelivery) Progress(ev runner.ProgressEvent) { t.a.Progress(ev); t.b.Progress(ev) }
func (t teeDelivery) Final(res runner.RunResult)       { t.a.Final(res); t.b.Final(res) }
func (t teeDelivery) Close()                           { t.a.Close(); t.b.Close() }

// deliveryFor builds the Delivery for a turn, picked by the chat's transport
// prefix. The "ui" prefix routes to the web-UI delivery. A messenger chat
// (tg/mx/vk) gets the edit-streaming messengerDelivery — and, when its session
// belongs to a canonical "user:" identity (so a web-UI client can be watching the
// same session), is ALSO mirrored to the UI so the answer streams there live, not
// only on reload. uiUserForKey gates the mirror to canonical sessions (raw
// tg:/group keys → no mirror); the UI ring buffers if no client is polling.
func (d *daemon) deliveryFor(ctx context.Context, msg queuedMsg, verbose bool) Delivery {
	if transportPrefix(msg.chatID) == uiPrefix {
		return d.newUIDelivery(ctx, msg)
	}
	md := d.newMessengerDelivery(ctx, msg, verbose)
	if d.uiHub != nil && uiUserForKey(msg.sessKey) != "" {
		return teeDelivery{a: md, b: d.newUIDelivery(ctx, msg)}
	}
	return md
}
