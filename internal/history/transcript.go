// Every transcript read goes through one incremental index per file. A record
// is parsed exactly once, when its terminating newline first appears, and every
// consumer — turn binding, the UI read model, transcript pages, context, audit —
// reads the same published snapshot, so an append is read and parsed once, not
// the whole history. Published items are immutable: an update that amends an
// already-published item (Codex token_count, compaction time) copies the item
// slice first. A transcript only grows (docs/CONTRACT.md); a path that now holds
// another file, or a file shorter than what was consumed, is re-indexed from zero.
package history

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"sync/atomic"
)

type rawRecord struct {
	Event int64
	Raw   []byte // valid only during the scanRecords callback
	Len   int64  // physical bytes, terminator included
}

func (r rawRecord) digest() string {
	sum := sha256.Sum256(r.Raw)
	return hex.EncodeToString(sum[:])
}

// scanRecords is the sole source of transcript event numbering: Event is the
// zero-based physical JSONL record index, regardless of record type or its
// embedded timestamp. Claude compact_boundary is an ordinary appended record in
// this same sequence; preserved summary/input rows written after it may carry
// earlier timestamps, so timestamps must never define turn ranges. It returns
// the bytes consumed through the last newline: a final unterminated fragment is
// left for a later read, after it is complete.
func scanRecords(r io.Reader, first int64, fn func(rawRecord)) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var consumed int64
	for ev := first; ; ev++ {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			line = slices.Clone(line)
			for err == bufio.ErrBufferFull {
				var more []byte
				more, err = br.ReadSlice('\n')
				line = append(line, more...)
			}
		}
		if err == io.EOF {
			return consumed, nil
		}
		if err != nil {
			return consumed, err
		}
		consumed += int64(len(line))
		fn(rawRecord{Event: ev, Raw: trimTerminator(line), Len: int64(len(line))})
	}
}

func trimTerminator(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	return bytes.TrimSuffix(line, []byte{'\r'})
}

type recordParser interface {
	add(rawRecord)
	publish() []Item // the items so far; the caller may keep them, the parser never mutates them again
}

func newParser(backend, sessionID string) recordParser {
	if backend == "codex" {
		return &codexParser{backend: backend, session: sessionID, lastAssistant: -1, lastCompacted: -1}
	}
	return &claudeParser{backend: backend, session: sessionID}
}

type transcriptSnapshot struct {
	path  string
	file  os.FileInfo // the indexed file; the path may later hold another
	items []Item
	ends  []int64 // end offset of each complete record
	gen   uint64  // changes only when the file is re-indexed from zero
}

type transcriptIndex struct {
	mu      sync.Mutex
	backend string
	session string
	info    os.FileInfo
	offset  int64
	ends    []int64
	gen     uint64
	parser  recordParser
	snap    transcriptSnapshot
}

var (
	transcripts sync.Map // path -> *transcriptIndex
	generations atomic.Uint64
	fullScans   sync.Mutex // indexing a whole file is done one file at a time
)

func loadTranscript(backend, sessionID, path string) (transcriptSnapshot, error) {
	v, _ := transcripts.LoadOrStore(path, &transcriptIndex{backend: backend, session: sessionID})
	x := v.(*transcriptIndex)
	x.mu.Lock()
	defer x.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			transcripts.CompareAndDelete(path, x)
		}
		return transcriptSnapshot{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return transcriptSnapshot{}, err
	}
	if x.info != nil && os.SameFile(fi, x.info) && fi.Size() == x.info.Size() && fi.ModTime().Equal(x.info.ModTime()) {
		return x.snap, nil
	}
	if x.info == nil || !os.SameFile(fi, x.info) || fi.Size() < x.offset {
		x.reset()
		fullScans.Lock()
		defer fullScans.Unlock()
	}
	_, err = scanRecords(io.NewSectionReader(f, x.offset, fi.Size()-x.offset), int64(len(x.ends)), func(rec rawRecord) {
		x.offset += rec.Len
		x.ends = append(x.ends, x.offset)
		x.parser.add(rec)
	})
	if err != nil {
		x.info = nil // the parser has consumed part of the tail; start over next time
		return transcriptSnapshot{}, err
	}
	x.info = fi
	x.snap = transcriptSnapshot{path: path, file: fi, items: x.parser.publish(), ends: x.ends[:len(x.ends):len(x.ends)], gen: x.gen}
	return x.snap, nil
}

func (x *transcriptIndex) reset() {
	x.offset, x.ends = 0, nil
	x.gen = generations.Add(1)
	x.parser = newParser(x.backend, x.session)
}
