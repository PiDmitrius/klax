package sessfiles

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ErrRemoved is returned by Enqueue/append after the session store has been removed
// (close/nuke), so an in-flight run's late Mark* cannot resurrect the directory.
var ErrRemoved = errors.New("sessfiles: session store removed")

// queue.jsonl is the per-session durable queue AND the session-lifetime inbound
// log: append-only records enq → run → done/err, never deleted. fsync on every
// append; enq is the durability point (a turn's files are fsynced first). Replay
// re-enqueues enq-without-run and flags run-without-terminal for transcript
// reconciliation. `turn_seq` is the monotonic canonical turn id. Legacy enqueues
// may carry a prompt marker; new runs bind to physical transcript coordinates.

type record struct {
	Ev           string   `json:"ev"` // enq|run|done|err
	Seq          int64    `json:"seq"`
	ChatID       string   `json:"chat,omitempty"` // originating chat, for replay delivery
	MsgID        string   `json:"msg,omitempty"`
	Nonce        string   `json:"nonce,omitempty"`
	Text         string   `json:"text,omitempty"`
	Files        []string `json:"files,omitempty"`
	Marker       string   `json:"marker,omitempty"`
	TS           int64    `json:"ts,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	Backend      string   `json:"backend,omitempty"`
	Session      string   `json:"session,omitempty"`
	PromptDigest string   `json:"prompt_digest,omitempty"`
	FromEvent    int64    `json:"from_event,omitempty"`
	Event        *int64   `json:"event,omitempty"`
	RecordDigest string   `json:"record_digest,omitempty"`
}

// Turn is a reconstructed inbound message: its enq fields plus its latest state.
type Turn struct {
	Seq          int64
	ChatID       string
	MsgID        string
	Nonce        string
	Text         string
	Files        []string // stored names (files/<name>)
	Marker       string
	TS           int64
	Last         string // enq|run|done|err
	Reason       string
	Backend      string
	Session      string
	PromptDigest string
	FromEvent    int64
	Bound        bool
	Event        int64
	RecordDigest string
	enqueued     bool
}

// NamedReader is one streaming file for Enqueue.
type NamedReader struct {
	Name string
	R    io.Reader
}

func (s *Store) queuePath() string { return filepath.Join(s.dir, "queue.jsonl") }

// QueueStat returns queue.jsonl's mod time and size (zero when absent) — a cheap change-detector
// for callers that cache something derived from the queue (e.g. the UI read model).
func (s *Store) QueueStat() (time.Time, int64) {
	fi, err := os.Stat(s.queuePath())
	if err != nil {
		return time.Time{}, 0
	}
	return fi.ModTime(), fi.Size()
}

// Enqueue durably accepts one inbound message: it reserves the next turn_seq,
// streams the files to disk (each fsynced), then appends a fsynced enq record.
// Returns the turn_seq, a legacy marker (empty for new turns), the stored
// file names, and whether this was a duplicate nonce that had already been accepted.
// Holds the durable-store lock across the whole acceptance, so turn_seq allocation,
// file writes and the enq append are one atomic unit.
func (s *Store) Enqueue(chatID, msgID, nonce, text string, files []NamedReader) (seq int64, marker string, stored []string, duplicate bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		err = ErrRemoved
		return
	}
	if err = s.ensureLoaded(); err != nil {
		return
	}
	if nonce != "" {
		var turns []Turn
		if turns, err = s.turns(); err != nil {
			return
		}
		for _, t := range turns {
			if t.Nonce == nonce {
				return t.Seq, t.Marker, append([]string(nil), t.Files...), true, nil
			}
		}
	}
	s.seq++ // reserve first: a failure below just burns the seq (gaps are fine)
	seq = s.seq
	for i, f := range files {
		var name string
		if name, err = s.WriteFile(seq, i+1, f.Name, f.R); err != nil {
			return
		}
		stored = append(stored, name)
	}
	err = s.appendRecord(record{Ev: "enq", Seq: seq, ChatID: chatID, MsgID: msgID, Nonce: nonce, Text: text, Files: stored, Marker: marker, TS: time.Now().UnixNano()})
	return
}

// MarkRun/MarkDone/MarkErr append progress/terminal records for a turn.
func (s *Store) MarkRun(seq int64) error { return s.mark(record{Ev: "run", Seq: seq}) }
func (s *Store) MarkRunMeta(seq int64, backend, session, promptDigest string, fromEvent int64) error {
	return s.mark(record{Ev: "run", Seq: seq, Backend: backend, Session: session, PromptDigest: promptDigest, FromEvent: fromEvent})
}
func (s *Store) MarkRunSession(seq int64, backend, session string, fromEvent int64) error {
	return s.mark(record{Ev: "run_session", Seq: seq, Backend: backend, Session: session, FromEvent: fromEvent})
}

var ErrBindConflict = errors.New("sessfiles: transcript binding conflict")

// Bind establishes an immutable one-to-one queue/transcript association.
func (s *Store) Bind(seq int64, backend, session string, event int64, recordDigest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	turns, err := s.turns()
	if err != nil {
		return err
	}
	var target *Turn
	for i := range turns {
		t := &turns[i]
		if t.Bound && t.Backend == backend && t.Session == session && t.Event == event {
			if t.Seq == seq && t.RecordDigest == recordDigest {
				return nil
			}
			return ErrBindConflict
		}
		if t.Seq == seq {
			target = t
		}
	}
	if target == nil || target.Backend != backend || target.Session != session {
		return ErrBindConflict
	}
	if target.Bound {
		return ErrBindConflict
	}
	for _, t := range turns {
		if !t.Bound || t.Backend != backend || t.Session != session {
			continue
		}
		if (t.Seq < seq && t.Event >= event) || (t.Seq > seq && t.Event <= event) {
			return ErrBindConflict
		}
	}
	return s.appendRecord(record{Ev: "bind", Seq: seq, Backend: backend, Session: session, Event: &event, RecordDigest: recordDigest, TS: time.Now().UnixNano()})
}
func (s *Store) MarkDone(seq int64) error { return s.mark(record{Ev: "done", Seq: seq}) }
func (s *Store) MarkErr(seq int64, reason string) error {
	return s.mark(record{Ev: "err", Seq: seq, Reason: reason})
}

func (s *Store) mark(rec record) error {
	rec.TS = time.Now().UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendRecord(rec)
}

// Replay reconstructs queue state after a restart: reenqueue = turns whose last
// record is enq (never reached the backend → safe to re-run); recover = turns
// whose last record is run (the backend may already have run side effects → the
// caller reconciles against the transcript by Marker and NEVER auto-reruns).
// Terminal (done/err) turns are skipped. Both lists are in turn_seq order.
func (s *Store) Replay() (reenqueue, recover []Turn, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turns, err := s.turns()
	if err != nil {
		return nil, nil, err
	}
	for _, t := range turns {
		switch t.Last {
		case "enq":
			reenqueue = append(reenqueue, t)
		case "run":
			recover = append(recover, t)
		}
	}
	return reenqueue, recover, nil
}

// InboundLog returns every accepted turn (enq text + files + marker), in turn_seq
// order, regardless of terminal state — the source for the reload-history merge.
func (s *Store) InboundLog() ([]Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turns()
}

// turns folds queue.jsonl records into per-seq Turns (latest state). Caller holds mu.
func (s *Store) turns() ([]Turn, error) {
	recs, err := s.readRecords()
	if err != nil {
		return nil, err
	}
	byseq := map[int64]*Turn{}
	for _, r := range recs {
		t := byseq[r.Seq]
		if t == nil {
			t = &Turn{Seq: r.Seq}
			byseq[r.Seq] = t
		}
		switch r.Ev {
		case "enq":
			t.ChatID, t.MsgID, t.Nonce, t.Text, t.Files, t.Marker, t.TS, t.Last =
				r.ChatID, r.MsgID, r.Nonce, r.Text, r.Files, r.Marker, r.TS, "enq"
			t.enqueued = true
		case "run":
			t.Last, t.Reason = r.Ev, r.Reason
			t.Backend, t.Session, t.PromptDigest, t.FromEvent = r.Backend, r.Session, r.PromptDigest, r.FromEvent
		case "run_session":
			if r.Backend != "" {
				t.Backend = r.Backend
			}
			t.Session, t.FromEvent = r.Session, r.FromEvent
		case "bind":
			if !t.Bound && r.Event != nil && t.Backend == r.Backend && t.Session == r.Session {
				t.Bound, t.Event, t.RecordDigest = true, *r.Event, r.RecordDigest
			}
		case "done", "err":
			t.Last, t.Reason = r.Ev, r.Reason
		}
	}
	out := make([]Turn, 0, len(byseq))
	for _, t := range byseq {
		if !t.enqueued { // no enq seen for this seq (torn/partial) — skip
			continue
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// readRecords scans queue.jsonl with a bufio.Reader (never a Scanner — matches the
// codebase rule and tolerates arbitrarily long lines). A torn trailing line from a
// crash mid-append fails to unmarshal and is skipped. Caller holds mu.
func (s *Store) readRecords() ([]record, error) {
	f, err := os.Open(s.queuePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var recs []record
	br := bufio.NewReader(f)
	for {
		line, rerr := br.ReadBytes('\n')
		if t := bytes.TrimRight(line, "\n"); len(t) > 0 {
			var r record
			if json.Unmarshal(t, &r) == nil {
				recs = append(recs, r)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return nil, rerr
		}
	}
	return recs, nil
}

// appendRecord writes one fsynced JSON line to queue.jsonl. Caller holds mu.
func (s *Store) appendRecord(r record) error {
	if s.removed {
		return ErrRemoved // a removed store must not be resurrected by a late append
	}
	if err := mkdirAllSync(s.dir); err != nil {
		return err
	}
	qp := s.queuePath()
	_, statErr := os.Stat(qp)
	newFile := os.IsNotExist(statErr)
	f, err := os.OpenFile(qp, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	// A crash can leave a partial final JSON record. Keep it as an ignored,
	// malformed physical line, but never concatenate the next valid record to it.
	if fi, statErr := f.Stat(); statErr != nil {
		return statErr
	} else if fi.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], fi.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if newFile {
		return fsyncDir(s.dir) // make the new queue.jsonl directory entry durable
	}
	return nil
}

// ensureLoaded sets s.seq to the highest turn_seq in the log. Caller holds mu.
func (s *Store) ensureLoaded() error {
	if s.loaded {
		return nil
	}
	recs, err := s.readRecords()
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.Seq > s.seq {
			s.seq = r.Seq
		}
	}
	s.loaded = true
	return nil
}
