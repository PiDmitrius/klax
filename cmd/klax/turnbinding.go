package main

import (
	"cmp"
	"fmt"
	"log"
	"slices"

	"github.com/PiDmitrius/klax/internal/history"
	"github.com/PiDmitrius/klax/internal/sessfiles"
)

type turnBinding struct {
	Seq              int64
	Backend, Session string
	Event            int64
	RecordDigest     string
}

func coordinateKey(backend, session string, event int64) string {
	return fmt.Sprintf("%s\x00%s\x00%d", backend, session, event)
}

// proposeBindings is the single ordered interval matcher used by persistence
// and by the read model's short-lived active-run provisional association. User
// records come in physical order, so each turn's interval is found by binary search.
func proposeBindings(turns []sessfiles.Turn, items []history.Item, backend, session string, end int64) []turnBinding {
	claimed := make(map[string]bool)
	for _, t := range turns {
		if t.Bound {
			claimed[coordinateKey(t.Backend, t.BackendID, t.Event)] = true
		}
	}
	var users []history.Item
	var out []turnBinding
	for i, t := range turns {
		if t.Bound || t.Backend != backend || t.BackendID != session || t.PromptDigest == "" {
			continue
		}
		if users == nil {
			users = make([]history.Item, 0, len(turns))
			for _, it := range items {
				if it.Role == "user" {
					users = append(users, it)
				}
			}
		}
		upper := end
		for j := i + 1; j < len(turns); j++ {
			n := turns[j]
			if n.Backend == backend && n.BackendID == session {
				upper = n.FromEvent
				break
			}
		}
		from, _ := slices.BinarySearchFunc(users, t.FromEvent, func(it history.Item, ev int64) int { return cmp.Compare(it.Event, ev) })
		for _, it := range users[from:] {
			if it.Event >= upper {
				break
			}
			if it.PromptDigest != t.PromptDigest {
				continue
			}
			key := coordinateKey(backend, session, it.Event)
			if claimed[key] {
				continue
			}
			claimed[key] = true
			out = append(out, turnBinding{Seq: t.Seq, Backend: backend, Session: session, Event: it.Event, RecordDigest: it.RecordDigest})
			break
		}
	}
	return out
}

// unboundBackendSessions lists, once each and in turn order, the backend sessions whose last
// turn is still unbound with the digest and transcript address the matcher needs. An earlier
// turn's interval was closed and reconciled when the next turn of that session started, and a
// transcript only grows, so it can never bind later; an all-bound session yields nothing,
// which keeps the startup pass free.
func unboundBackendSessions(turns []sessfiles.Turn) [][2]string {
	last := make(map[[2]string]sessfiles.Turn)
	var order [][2]string
	for _, t := range turns {
		if t.Backend == "" || t.BackendID == "" {
			continue
		}
		key := [2]string{t.Backend, t.BackendID}
		if _, ok := last[key]; !ok {
			order = append(order, key)
		}
		last[key] = t
	}
	var out [][2]string
	for _, key := range order {
		if t := last[key]; !t.Bound && t.PromptDigest != "" {
			out = append(out, key)
		}
	}
	return out
}

// bindingRepair is one backend session to re-match, addressed by the klax session that owns it.
type bindingRepair struct {
	sk               string
	klaxID           string
	cwd              string
	backend, session string
}

func bindingRepairs(sk string, klaxID string, cwd string, turns []sessfiles.Turn) []bindingRepair {
	var out []bindingRepair
	for _, bs := range unboundBackendSessions(turns) {
		out = append(out, bindingRepair{sk: sk, klaxID: klaxID, cwd: cwd, backend: bs[0], session: bs[1]})
	}
	return out
}

// repairBindings matches a run whose bind never landed — the daemon died between the
// transcript append and the bind fsync, or klax could not read the record when it was
// written — so its answer joins its durable turn. It reads whole backend transcripts and a
// transport must never wait on that, so it runs off the startup path; racing a live run is
// safe because Store.Bind is the single one-to-one authority and rejects a conflict.
func (d *daemon) repairBindings(work []bindingRepair) {
	for _, w := range work {
		if d.reconcileBindings(w.sk, w.klaxID, w.backend, w.session, w.cwd) {
			// A repaired turn changes an already-rendered answer, and unlike the lifecycle
			// call sites nothing else here wakes the surfaces afterwards.
			d.broadcastSessions(w.sk)
		}
	}
}

// reconcileBindings reports whether a proposed binding is now durably present.
func (d *daemon) reconcileBindings(sk string, klaxID string, backend, sessionID, cwd string) bool {
	if sessionID == "" {
		return false
	}
	items, end, err := history.Snapshot(backend, sessionID, cwd)
	if err != nil {
		log.Printf("turn binding transcript %s/%s: %v", sk, klaxID, err)
		return false
	}
	return d.reconcileBindingsSnapshot(sk, klaxID, backend, sessionID, items, end)
}

func (d *daemon) reconcileBindingsSnapshot(sk string, klaxID string, backend, sessionID string, items []history.Item, end int64) bool {
	st := d.sessionStore(sk, klaxID)
	turns, err := st.InboundLog()
	if err != nil {
		log.Printf("turn binding queue %s/%s: %v", sk, klaxID, err)
		return false
	}
	var bound bool
	for _, b := range proposeBindings(turns, items, backend, sessionID, end) {
		if err := st.Bind(b.Seq, b.Backend, b.Session, b.Event, b.RecordDigest); err == nil {
			bound = true
		} else if err == sessfiles.ErrBindConflict {
			log.Printf("turn bind conflict %s/%s turn %d event %d", sk, klaxID, b.Seq, b.Event)
		} else {
			log.Printf("turn bind %s/%s turn %d: %v", sk, klaxID, b.Seq, err)
		}
	}
	return bound
}
