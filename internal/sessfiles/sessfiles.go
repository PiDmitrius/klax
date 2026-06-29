// Package sessfiles owns a session's working directory under the klax data dir
// (<data>/sessions/<keydir>/<created>/): durable inbound/outbound files named by
// turn, and — wired later — the durable queue log.
//
// Files are named "<turn_seq>-<NN>-name.ext": the turn id (allocated by the
// durable-queue layer, passed in here) and a 1-based per-turn file index, so
// several files in one message never collide. Turn identity is recorded
// explicitly in the queue log, NEVER inferred from a file name. Writes stream
// (no whole-file buffering), are durable (temp → fsync → exclusive link → dir
// fsync) and idempotent on replay. The agent never sees these paths — Materialize
// copies a clean per-turn view (the "<seq>-<NN>-" prefix stripped); a whole
// session's files are owned by one (key, created), so cleanup is one RemoveAll.
package sessfiles

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/PiDmitrius/klax/internal/session"
)

// keyDir maps a sessionKey to one safe, injective path component
// "<hint>--<base64url(raw key)>". The base64url suffix is lossless, so the
// component is unique per key regardless of the cosmetic hint; base64url's
// alphabet ([A-Za-z0-9_-]) is filesystem-safe. (A char-replacement sanitizer is
// NOT injective — e.g. "a:b" and "a/b" would collide — so it is not used here.)
func keyDir(key string) string {
	return keyHint(key) + "--" + base64.RawURLEncoding.EncodeToString([]byte(key))
}

// keyHint is a short, sanitized, lossy label for human eyes only (ls legibility).
func keyHint(key string) string {
	h := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, key)
	if len(h) > 32 {
		h = h[:32]
	}
	return h
}

// WorkDir is the per-session directory: <data>/sessions/<keyDir>/<created>.
func WorkDir(key string, created int64) string {
	return filepath.Join(session.StoreDir(), "sessions", keyDir(key), strconv.FormatInt(created, 10))
}

// Store is a session's durable store: its files/ subdir and its queue.jsonl, plus
// the per-session durable-store lock and the cached turn_seq high-water. One Store
// per (key, created); the daemon keeps it on the sessionRunner. The durable-store
// lock (mu) is DISTINCT from the runner's sr.mu — never held across runner waits.
type Store struct {
	dir     string
	mu      sync.Mutex
	seq     int64 // turn_seq high-water, lazily loaded from queue.jsonl
	loaded  bool
	removed bool // set by Remove; afterwards Enqueue/append return ErrRemoved (no resurrection)
}

// Open binds a Store to a session. No I/O — directories are created lazily.
func Open(key string, created int64) *Store { return &Store{dir: WorkDir(key, created)} }

func (s *Store) filesDir() string { return filepath.Join(s.dir, "files") }

// Path returns the absolute path of a stored file (by its stored base name).
func (s *Store) Path(stored string) string { return filepath.Join(s.filesDir(), stored) }

// storedName builds "<turnSeq>-<NN>-name.ext" (turnSeq zero-padded for
// chronological ls; NN the 1-based file index within the turn).
func storedName(turnSeq int64, idx int, origName string) string {
	return fmt.Sprintf("%06d-%02d-%s", turnSeq, idx, Sanitize(origName))
}

// WriteFile durably writes one file for a turn, streaming from r into
// files/<turnSeq>-<NN>-name.ext, and returns the stored base name. Durability:
// temp in the same dir → fsync → exclusive link into place → fsync(dir). It is
// idempotent: if the exact slot already exists (replay, or a lost race), it is
// left as-is. Streaming (io.Reader) so the caller never buffers a whole file.
func (s *Store) WriteFile(turnSeq int64, idx int, name string, r io.Reader) (string, error) {
	return s.putDurable(storedName(turnSeq, idx, name), r)
}

// Adopt copies an agent-produced output file into the durable store under a
// content-addressed name "out-<hash>-name.ext" (immutable + dedup'd), so the served
// copy survives the agent later rewriting or deleting the original. Returns the
// stored base name. Holds the durable-store lock (so a concurrent close latch is
// honored: a removed store returns ErrRemoved rather than being resurrected).
func (s *Store) Adopt(name, srcPath string) (string, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		return "", err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	stored := "out-" + hex.EncodeToString(h.Sum(nil)[:16]) + "-" + Sanitize(name) // 128-bit, collision-safe
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return "", ErrRemoved
	}
	return s.putDurable(stored, src)
}

// putDurable writes r to files/<stored> durably (temp -> fsync -> exclusive link ->
// dir fsync); idempotent if the slot already exists. WriteFile is serialized by
// Enqueue's lock; Adopt holds s.mu itself.
func (s *Store) putDurable(stored string, r io.Reader) (string, error) {
	dir := s.filesDir()
	if err := mkdirAllSync(dir); err != nil {
		return "", err
	}
	final := filepath.Join(dir, stored)
	if _, err := os.Lstat(final); err == nil {
		return stored, nil // already present (idempotent)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // cleans the temp on error and after a successful link
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// Exclusive publish: link fails EEXIST if a concurrent writer already created the
	// slot — that is the idempotent outcome, not an error.
	if err := os.Link(tmpName, final); err != nil {
		if os.IsExist(err) {
			return stored, nil
		}
		return "", err
	}
	return stored, fsyncDir(dir)
}

// Blob is one in-memory file for WriteTurn.
type Blob struct {
	Name string
	Data []byte
}

// WriteTurn writes all of a turn's files (1-based index) and returns the stored
// names in order. Convenience over WriteFile for in-memory blobs.
func (s *Store) WriteTurn(turnSeq int64, blobs []Blob) ([]string, error) {
	out := make([]string, 0, len(blobs))
	for i, b := range blobs {
		stored, err := s.WriteFile(turnSeq, i+1, b.Name, bytes.NewReader(b.Data))
		if err != nil {
			return out, err
		}
		out = append(out, stored)
	}
	return out, nil
}

var storedPrefix = regexp.MustCompile(`^\d+-\d+-`)

func stripPrefix(name string) string { return storedPrefix.ReplaceAllString(name, "") }

// DisplayName returns a stored file's original-ish name (the "<turnSeq>-<NN>-" prefix
// stripped), e.g. "000001-01-photo.png" -> "photo.png", for showing in the UI.
func DisplayName(stored string) string { return stripPrefix(stored) }

// Materialize presents stored files to the agent under a clean per-turn dir: the
// "<seq>-<NN>-" prefix is stripped and within-dir name clashes get a "-N" suffix
// (image.png → image-2.png). It copies the bytes (not a symlink, whose realpath
// would expose the internal durable path; not a hardlink, since the run dir may be
// tmpfs = EXDEV). Returns the clean paths in dst, in input order.
func (s *Store) Materialize(stored []string, dst string) ([]string, error) {
	if err := os.MkdirAll(dst, 0700); err != nil {
		return nil, err
	}
	used := map[string]bool{}
	out := make([]string, 0, len(stored))
	for _, name := range stored {
		dstPath := filepath.Join(dst, dedup(stripPrefix(name), used))
		if err := copyFile(s.Path(name), dstPath); err != nil {
			return out, err
		}
		out = append(out, dstPath)
	}
	return out, nil
}

// Remove deletes the whole session work dir (files + queue) and latches the store
// closed under mu, so a late Mark*/Enqueue from an in-flight run returns ErrRemoved
// instead of recreating the directory (orphan). Used on close/nuke.
func (s *Store) Remove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = true
	return os.RemoveAll(s.dir)
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// dedup returns a name not yet in used, inserting "-N" before the extension on a
// clash, and records the result.
func dedup(name string, used map[string]bool) string {
	if !used[name] {
		used[name] = true
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		if cand := fmt.Sprintf("%s-%d%s", base, i, ext); !used[cand] {
			used[cand] = true
			return cand
		}
	}
}

// mkdirAllSync creates dir like os.MkdirAll and fsyncs the parent of every level it
// newly creates, so the new directory entries survive a crash (a per-file fsync is
// not enough if the directory entry itself was never flushed).
func mkdirAllSync(dir string) error {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	var missing []string
	for d := dir; ; {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append(missing, d)
		if parent := filepath.Dir(d); parent != d {
			d = parent
		} else {
			break
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, d := range missing {
		if err := fsyncDir(filepath.Dir(d)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Sanitize reduces a user/agent-supplied filename to a safe base name. Mirrors
// cmd/klax.sanitizeAttachmentFilename, plus strips control characters (the name
// becomes a run-view directory entry and appears in prompt text). The two will be
// unified onto this when the intake path is wired.
func Sanitize(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return "attachment"
	}
	return name
}
