package sessfiles

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
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
// reconciliation. `turn_seq` is the monotonic turn id; `turn_marker` is the opaque
// token klax injects into the prompt to correlate the backend transcript turn.

type record struct {
	Ev     string   `json:"ev"` // enq|run|done|err
	Seq    int64    `json:"seq"`
	ChatID string   `json:"chat,omitempty"` // originating chat, for replay delivery
	MsgID  string   `json:"msg,omitempty"`
	Nonce  string   `json:"nonce,omitempty"`
	Text   string   `json:"text,omitempty"`
	Files  []string `json:"files,omitempty"`
	Marker string   `json:"marker,omitempty"`
	TS     int64    `json:"ts,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// Turn is a reconstructed inbound message: its enq fields plus its latest state.
type Turn struct {
	Seq    int64
	ChatID string
	MsgID  string
	Nonce  string
	Text   string
	Files  []string // stored names (files/<name>)
	Marker string
	TS     int64
	Last   string // enq|run|done|err
	Reason string
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
// Returns the turn_seq, the opaque turn_marker to inject into the prompt, the stored
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
	if marker, err = newMarker(); err != nil {
		return
	}
	err = s.appendRecord(record{Ev: "enq", Seq: seq, ChatID: chatID, MsgID: msgID, Nonce: nonce, Text: text, Files: stored, Marker: marker, TS: time.Now().UnixNano()})
	return
}

// MarkRun/MarkDone/MarkErr append progress/terminal records for a turn.
func (s *Store) MarkRun(seq int64) error  { return s.mark(record{Ev: "run", Seq: seq}) }
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
		case "run", "done", "err":
			t.Last, t.Reason = r.Ev, r.Reason
		}
	}
	out := make([]Turn, 0, len(byseq))
	for _, t := range byseq {
		if t.Marker == "" { // no enq seen for this seq (torn/partial) — skip
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
	f, err := os.OpenFile(qp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
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

func newMarker() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
