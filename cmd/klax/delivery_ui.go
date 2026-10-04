package main

import (
	"context"

	"github.com/PiDmitrius/klax/internal/runner"
)

// uiDelivery wakes the web UI's held polls as a turn progresses. It carries NO content: the live
// channel's detector reads the turn's rows from the durable log (queue ⋈ transcript) via
// buildReadModel — the one path shared by live delivery and reload — so delivery just POKES.
type uiDelivery struct {
	d    *daemon
	user string // canonical user (hub key)
}

func (d *daemon) newUIDelivery(_ context.Context, msg queuedMsg) *uiDelivery {
	u := &uiDelivery{d: d, user: uiUserForKey(msg.sessKey)}
	u.d.uiPoke(u.user) // turn started (its state is in the durable queue) → wake the held polls
	return u
}

func (u *uiDelivery) Progress(runner.ProgressEvent) { u.d.uiPoke(u.user) }
func (u *uiDelivery) Warning(string)                { u.d.uiPoke(u.user) }
func (u *uiDelivery) Final(runner.RunResult)        { u.d.uiPoke(u.user) }
func (u *uiDelivery) Close()                        {}
