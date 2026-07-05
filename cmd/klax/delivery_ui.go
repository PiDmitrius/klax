package main

import (
	"context"

	"github.com/PiDmitrius/klax/internal/runner"
)

// uiDelivery wakes the web UI's tail-poll as a turn progresses. It carries NO content: the tail
// reads the turn's rows from the durable log (queue ⋈ transcript) via buildReadModel — the one path
// shared by live delivery and reload — so delivery just POKES the held poll to re-read. (Block ids,
// tool previews and outbound file-ref rewriting now live only in buildReadModel.)
type uiDelivery struct {
	d    *daemon
	user string // canonical user (hub key)
}

func (d *daemon) newUIDelivery(_ context.Context, msg queuedMsg) *uiDelivery {
	u := &uiDelivery{d: d, user: uiUserForKey(msg.sessKey)}
	u.d.uiPoke(u.user) // turn started (its state is in the durable queue) → wake the tail
	return u
}

func (u *uiDelivery) Progress(runner.ProgressEvent) { u.d.uiPoke(u.user) }
func (u *uiDelivery) Final(runner.RunResult)        { u.d.uiPoke(u.user) }
func (u *uiDelivery) Close()                        {}
