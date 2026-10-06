package sessfiles

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/PiDmitrius/klax/internal/inbound"
)

// ErrRemoved is returned by Enqueue/append after the session store has been removed
// (close/nuke), so an in-flight run's late Mark* cannot resurrect the directory.
var ErrRemoved = errors.New("sessfiles: session store removed")

// queue.jsonl is the per-session durable queue AND the session-lifetime inbound
// log: append-only records reserve → enq → run → done/err, never deleted. fsync on every
// append; reserve burns the sequence before files are written, and enq accepts the turn.
// A turn's files are fsynced first. Replay
// re-enqueues enq-without-run and flags run-without-terminal for transcript
// reconciliation. `turn_seq` is the monotonic canonical turn id. Legacy enqueues
// may carry a prompt marker; new runs bind to physical transcript coordinates.

type record struct {
	Ev           string         `json:"ev"` // reserve|enq|run|run_session|bind|done|err|hook
	Seq          int64          `json:"turn_seq"`
	ChatID       string         `json:"chat,omitempty"` // originating chat, for replay delivery
	MsgID        string         `json:"msg,omitempty"`
	Nonce        string         `json:"nonce,omitempty"`
	Text         string         `json:"text,omitempty"`
	OriginalText string         `json:"original_text,omitempty"`
	Files        []string       `json:"files,omitempty"`
	Marker       string         `json:"marker,omitempty"`
	TS           int64          `json:"ts,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	CtxWindow    int            `json:"ctx_window,omitempty"`
	Hook         string         `json:"hook,omitempty"`
	Status       string         `json:"status,omitempty"`
	Backend      string         `json:"backend,omitempty"`
	BackendID    string         `json:"backend_id,omitempty"`
	PromptDigest string         `json:"prompt_digest,omitempty"`
	FromEvent    int64          `json:"from_event,omitempty"`
	Event        *int64         `json:"event,omitempty"`
	RecordDigest string         `json:"record_digest,omitempty"`
	Origin       inbound.Origin `json:"origin,omitempty"`
}

// Turn is a reconstructed inbound message: its enq fields plus its latest state.
type Turn struct {
	Seq          int64
	ChatID       string
	MsgID        string
	Nonce        string
	Text         string
	OriginalText string
	Files        []string // stored names (files/<name>)
	Marker       string
	TS           int64
	Last         string // enq|run|done|err
	Reason       string
	CtxWindow    int // context window the turn ran with, from its terminal record
	HookFailures []HookFailure
	Backend      string
	BackendID    string
	PromptDigest string
	FromEvent    int64
	Bound        bool
	Event        int64
	RecordDigest string
	Origin       inbound.Origin
	enqueued     bool
}

// HookFailure is a durable hook diagnostic associated with a turn. Start-hook
// failure is terminal; finish-hook failure is an additive warning and does not
// rewrite the completed backend result.
type HookFailure struct {
	Hook   string
	Status string
	Reason string
	TS     int64
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
	seq, marker, stored, duplicate, _, err = s.EnqueueOrigin(chatID, msgID, nonce, text, text, files, inbound.Origin{})
	return
}

// EnqueueOrigin is Enqueue plus the normalized immutable message origin needed
// by consumers at run time, including after durable replay. acceptedAt is the
// exact timestamp written to the enq record.
func (s *Store) EnqueueOrigin(chatID, msgID, nonce, text, originalText string, files []NamedReader, origin inbound.Origin) (seq int64, marker string, stored []string, duplicate bool, acceptedAt int64, err error) {
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
				return t.Seq, t.Marker, append([]string(nil), t.Files...), true, t.TS, nil
			}
		}
	}
	s.seq++
	seq = s.seq
	if err = s.appendRecord(record{Ev: "reserve", Seq: seq}); err != nil {
		return
	}
	for i, f := range files {
		var name string
		if name, err = s.WriteFile(seq, i+1, f.Name, f.R); err != nil {
			return
		}
		stored = append(stored, name)
	}
	acceptedAt = time.Now().UnixNano()
	err = s.appendRecord(record{Ev: "enq", Seq: seq, ChatID: chatID, MsgID: msgID, Nonce: nonce, Text: text, OriginalText: originalText, Files: stored, Marker: marker, TS: acceptedAt, Origin: origin})
	return
}

// MarkRun/MarkDone/MarkErr append progress/terminal records for a turn.
func (s *Store) MarkRun(seq int64) error { return s.mark(record{Ev: "run", Seq: seq}) }
func (s *Store) MarkRunMeta(seq int64, backend, backendID, promptDigest string, fromEvent int64) error {
	return s.mark(record{Ev: "run", Seq: seq, Backend: backend, BackendID: backendID, PromptDigest: promptDigest, FromEvent: fromEvent})
}
func (s *Store) MarkRunSession(seq int64, backend, backendID string, fromEvent int64) error {
	return s.mark(record{Ev: "run_session", Seq: seq, Backend: backend, BackendID: backendID, FromEvent: fromEvent})
}

var ErrBindConflict = errors.New("sessfiles: transcript binding conflict")

// Bind establishes an immutable one-to-one queue/transcript association.
func (s *Store) Bind(seq int64, backend, backendID string, event int64, recordDigest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	turns, err := s.turns()
	if err != nil {
		return err
	}
	var target *Turn
	for i := range turns {
		t := &turns[i]
		if t.Bound && t.Backend == backend && t.BackendID == backendID && t.Event == event {
			if t.Seq == seq && t.RecordDigest == recordDigest {
				return nil
			}
			return ErrBindConflict
		}
		if t.Seq == seq {
			target = t
		}
	}
	if target == nil || target.Backend != backend || target.BackendID != backendID {
		return ErrBindConflict
	}
	if target.Bound {
		return ErrBindConflict
	}
	for _, t := range turns {
		if !t.Bound || t.Backend != backend || t.BackendID != backendID {
			continue
		}
		if (t.Seq < seq && t.Event >= event) || (t.Seq > seq && t.Event <= event) {
			return ErrBindConflict
		}
	}
	return s.appendRecord(record{Ev: "bind", Seq: seq, Backend: backend, BackendID: backendID, Event: &event, RecordDigest: recordDigest, TS: time.Now().UnixNano()})
}
func (s *Store) MarkDone(seq int64, ctxWindow int) error {
	return s.mark(record{Ev: "done", Seq: seq, CtxWindow: ctxWindow})
}
func (s *Store) MarkErr(seq int64, reason string, ctxWindow int) error {
	return s.mark(record{Ev: "err", Seq: seq, Reason: reason, CtxWindow: ctxWindow})
}
func (s *Store) MarkHookError(seq int64, hook, reason string) error {
	return s.mark(record{Ev: "hook", Seq: seq, Hook: hook, Status: "error", Reason: reason})
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

// turns returns the per-seq Turns (latest state) in turn_seq order, as a copy the caller may
// modify. Caller holds mu.
func (s *Store) turns() ([]Turn, error) {
	if err := s.refreshQueue(); err != nil {
		return nil, err
	}
	out := slices.Clone(s.queue.turns)
	for i := range out {
		out[i].Files, out[i].HookFailures = slices.Clone(out[i].Files), slices.Clone(out[i].HookFailures)
	}
	return out, nil
}

// queueProjection is queue.jsonl folded into per-seq Turns. queue.jsonl only grows, so the
// projection reads just the complete records appended since the last read; a path that now
// holds another file, or a shorter one, is folded again from zero.
type queueProjection struct {
	info   os.FileInfo
	offset int64
	byseq  map[int64]*Turn
	maxSeq int64
	turns  []Turn // sorted snapshot of byseq, rebuilt when a record is folded
}

// refreshQueue folds the records appended to queue.jsonl since the last call. A torn line
// from a crash mid-append fails to unmarshal and is skipped; a complete record whose newline
// was lost counts, as appendRecord only terminates it before the next one. Caller holds mu.
func (s *Store) refreshQueue() error {
	q := &s.queue
	f, err := os.Open(s.queuePath())
	if err != nil {
		if os.IsNotExist(err) {
			*q = queueProjection{}
			return nil
		}
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if q.info != nil && os.SameFile(fi, q.info) && fi.Size() == q.info.Size() && fi.ModTime().Equal(q.info.ModTime()) {
		return nil
	}
	if q.info == nil || !os.SameFile(fi, q.info) || fi.Size() < q.offset {
		*q = queueProjection{byseq: map[int64]*Turn{}}
	}
	br := bufio.NewReader(io.NewSectionReader(f, q.offset, fi.Size()-q.offset))
	folded := false
	for {
		line, rerr := br.ReadBytes('\n')
		if rerr != nil && rerr != io.EOF {
			q.info = nil
			return rerr
		}
		var r record
		valid := json.Unmarshal(bytes.TrimRight(line, "\n"), &r) == nil
		if rerr == io.EOF && !valid {
			break // an unterminated partial record is read once it is complete
		}
		q.offset += int64(len(line))
		if valid {
			q.fold(r)
			folded = true
		}
		if rerr == io.EOF {
			break
		}
	}
	q.info = fi
	if folded || q.turns == nil {
		q.turns = make([]Turn, 0, len(q.byseq))
		for _, t := range q.byseq {
			if t.enqueued { // no enq seen for this seq (torn/partial) — skip
				q.turns = append(q.turns, *t)
			}
		}
		sort.Slice(q.turns, func(i, j int) bool { return q.turns[i].Seq < q.turns[j].Seq })
	}
	return nil
}

func (q *queueProjection) fold(r record) {
	if r.Seq > q.maxSeq {
		q.maxSeq = r.Seq
	}
	if r.Ev == "reserve" {
		return
	}
	t := q.byseq[r.Seq]
	if t == nil {
		t = &Turn{Seq: r.Seq}
		q.byseq[r.Seq] = t
	}
	switch r.Ev {
	case "enq":
		t.ChatID, t.MsgID, t.Nonce, t.Text, t.Files, t.Marker, t.TS, t.Last =
			r.ChatID, r.MsgID, r.Nonce, r.Text, r.Files, r.Marker, r.TS, "enq"
		t.OriginalText = r.OriginalText
		if t.OriginalText == "" {
			t.OriginalText = r.Text
		}
		t.Origin = r.Origin
		t.enqueued = true
	case "run":
		t.Last, t.Reason = r.Ev, r.Reason
		t.Backend, t.BackendID, t.PromptDigest, t.FromEvent = r.Backend, r.BackendID, r.PromptDigest, r.FromEvent
	case "run_session":
		if r.Backend != "" {
			t.Backend = r.Backend
		}
		t.BackendID, t.FromEvent = r.BackendID, r.FromEvent
	case "bind":
		if !t.Bound && r.Event != nil && t.Backend == r.Backend && t.BackendID == r.BackendID {
			t.Bound, t.Event, t.RecordDigest = true, *r.Event, r.RecordDigest
		}
	case "done", "err":
		t.Last, t.Reason, t.CtxWindow = r.Ev, r.Reason, r.CtxWindow
	case "hook":
		if r.Status != "error" {
			return
		}
		t.HookFailures = append(t.HookFailures, HookFailure{
			Hook: r.Hook, Status: r.Status, Reason: r.Reason, TS: r.TS,
		})
		if r.Hook == "audit.turn.start" {
			t.Last, t.Reason = "err", r.Reason
		}
	}
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
	if err := s.refreshQueue(); err != nil {
		return err
	}
	s.seq = max(s.seq, s.queue.maxSeq)
	s.loaded = true
	return nil
}
