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

func (d *daemon) reconcileBindings(sk string, created int64, backend, sessionID, cwd string) {
	if sessionID == "" {
		return
	}
	items, end, err := history.Snapshot(backend, sessionID, cwd)
	if err != nil {
		log.Printf("turn binding transcript %s/%d: %v", sk, created, err)
		return
	}
	st := d.sessionStore(sk, created)
	turns, err := st.InboundLog()
	if err != nil {
		log.Printf("turn binding queue %s/%d: %v", sk, created, err)
		return
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
	for _, b := range proposeBindings(turns, items, backend, sessionID, end) {
		if err := st.Bind(b.Seq, b.Backend, b.Session, b.Event, b.RecordDigest); err != nil && err != sessfiles.ErrBindConflict {
			log.Printf("turn bind %s/%d turn %d: %v", sk, created, b.Seq, err)
		} else if err == sessfiles.ErrBindConflict {
			log.Printf("turn bind conflict %s/%d turn %d event %d", sk, created, b.Seq, b.Event)
		}
	}
}
