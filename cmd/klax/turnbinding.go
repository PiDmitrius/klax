package main

import (
	"fmt"
	"log"

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
// and by the read model's short-lived active-run provisional association.
func proposeBindings(turns []sessfiles.Turn, items []history.Item, backend, session string, end int64) []turnBinding {
	claimed := make(map[string]bool)
	for _, t := range turns {
		if t.Bound {
			claimed[coordinateKey(t.Backend, t.Session, t.Event)] = true
		}
	}
	var out []turnBinding
	for i, t := range turns {
		if t.Bound || t.Backend != backend || t.Session != session || t.PromptDigest == "" {
			continue
		}
		upper := end
		for j := i + 1; j < len(turns); j++ {
			n := turns[j]
			if n.Backend == backend && n.Session == session {
				upper = n.FromEvent
				break
			}
		}
		for _, it := range items {
			if it.Role != "user" || it.Event < t.FromEvent || it.Event >= upper || it.PromptDigest != t.PromptDigest {
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

// unboundBackendSessions lists, once each and in turn order, the backend sessions that still
// own a turn a bind could match: unbound, with the digest and transcript address the matcher
// needs. An all-bound session yields nothing, which is what keeps the startup pass free.
func unboundBackendSessions(turns []sessfiles.Turn) [][2]string {
	seen := make(map[string]bool)
	var out [][2]string
	for _, t := range turns {
		if t.Bound || t.PromptDigest == "" || t.Backend == "" || t.Session == "" {
			continue
		}
		key := t.Backend + "\x00" + t.Session
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, [2]string{t.Backend, t.Session})
	}
	return out
}

// bindingRepair is one backend session to re-match, addressed by the klax session that owns it.
type bindingRepair struct {
	sk               string
	created          int64
	cwd              string
	backend, session string
}

func bindingRepairs(sk string, created int64, cwd string, turns []sessfiles.Turn) []bindingRepair {
	var out []bindingRepair
	for _, bs := range unboundBackendSessions(turns) {
		out = append(out, bindingRepair{sk: sk, created: created, cwd: cwd, backend: bs[0], session: bs[1]})
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
		if d.reconcileBindings(w.sk, w.created, w.backend, w.session, w.cwd) {
			// A repaired turn changes an already-rendered answer, and unlike the lifecycle
			// call sites nothing else here wakes the surfaces afterwards.
			d.broadcastSessions(w.sk)
		}
	}
}

// reconcileBindings reports whether a proposed binding is now durably present.
func (d *daemon) reconcileBindings(sk string, created int64, backend, sessionID, cwd string) bool {
	if sessionID == "" {
		return false
	}
	items, end, err := history.Snapshot(backend, sessionID, cwd)
	if err != nil {
		log.Printf("turn binding transcript %s/%d: %v", sk, created, err)
		return false
	}
	return d.reconcileBindingsSnapshot(sk, created, backend, sessionID, items, end)
}

func (d *daemon) reconcileBindingsSnapshot(sk string, created int64, backend, sessionID string, items []history.Item, end int64) bool {
	st := d.sessionStore(sk, created)
	turns, err := st.InboundLog()
	if err != nil {
		log.Printf("turn binding queue %s/%d: %v", sk, created, err)
		return false
	}
	records := make(map[int64]string)
	for _, it := range items {
		if it.RecordDigest != "" {
			records[it.Event] = it.RecordDigest
		}
	}
	for _, t := range turns {
		if !t.Bound || t.Backend != backend || t.Session != sessionID || t.Event >= end {
			continue
		}
		if actual := records[t.Event]; actual != t.RecordDigest {
			log.Printf("turn binding changed %s/%d turn %d %s/%s event %d: expected %s actual %s", sk, created, t.Seq, backend, sessionID, t.Event, t.RecordDigest, actual)
		}
	}
	var bound bool
	for _, b := range proposeBindings(turns, items, backend, sessionID, end) {
		if err := st.Bind(b.Seq, b.Backend, b.Session, b.Event, b.RecordDigest); err == nil {
			bound = true
		} else if err == sessfiles.ErrBindConflict {
			log.Printf("turn bind conflict %s/%d turn %d event %d", sk, created, b.Seq, b.Event)
		} else {
			log.Printf("turn bind %s/%d turn %d: %v", sk, created, b.Seq, err)
		}
	}
	return bound
}
