package sessfiles

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// links.json is a session's file index. Invariants:
//
//   - links:   blob -> access token. The token is minted once and never re-minted, so a rendered
//     <img src> is stable across rebuilds and restarts.
//   - sources: "<turnSeq>:<abs source path>" -> blob. What a given turn's link resolves to. A later
//     turn linking the same path gets its own entry, so an edited file shows up in the new
//     answer while the old one keeps serving what it delivered.
//   - seen:    abs source path -> blob + (dev, ino, size, mtime, ctime). Content identity, so an
//     unchanged file is not read again. A file written within racyWindow is never trusted.
//
// The file is written whole, atomically (temp -> fsync -> rename -> fsync dir), and cached in memory
// keyed by its own (mtime, size). Every mutation clones before writing, so a failed write leaves the
// cache matching disk. A blob named in any map may have been deleted; every lookup verifies it.

// LinkEntry is one file's access token plus what serving it needs.
type LinkEntry struct {
	Token       string `json:"token"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
}

type linksFile struct {
	Links   map[string]LinkEntry `json:"links"`
	Sources map[string]string    `json:"sources,omitempty"`
	Seen    map[string]SeenEntry `json:"seen,omitempty"`
}

// SeenEntry records one path's identity at the moment its content was hashed.
type SeenEntry struct {
	Blob    string `json:"blob"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
	CtimeNS int64  `json:"ctime_ns"`
	Ino     uint64 `json:"ino"`
	Dev     uint64 `json:"dev"`
}

func (s *Store) linksPath() string { return filepath.Join(s.dir, "links.json") }

// loadLinks returns links.json's contents, from the cache when the file is unchanged.
// Caller holds s.mu.
func (s *Store) loadLinks() (linksFile, error) {
	mtime, size := s.linksStat()
	if s.links != nil && s.linksMtime.Equal(mtime) && s.linksSize == size {
		return *s.links, nil
	}
	lf := linksFile{Links: map[string]LinkEntry{}, Sources: map[string]string{}, Seen: map[string]SeenEntry{}}
	data, err := os.ReadFile(s.linksPath())
	if os.IsNotExist(err) {
		s.cacheLinks(&lf)
		return lf, nil
	}
	if err != nil {
		return lf, err
	}
	if err := json.Unmarshal(data, &lf); err != nil {
		return lf, err
	}
	if lf.Links == nil {
		lf.Links = map[string]LinkEntry{}
	}
	if lf.Sources == nil {
		lf.Sources = map[string]string{}
	}
	if lf.Seen == nil {
		lf.Seen = map[string]SeenEntry{}
	}
	s.cacheLinks(&lf)
	return lf, nil
}

// linksStat keys the cache. A missing file yields the zero value, which no write can produce.
func (s *Store) linksStat() (time.Time, int64) {
	fi, err := os.Stat(s.linksPath())
	if err != nil {
		return time.Time{}, 0
	}
	return fi.ModTime(), fi.Size()
}

// cacheLinks records lf as the cached view with the file identity it came from.
func (s *Store) cacheLinks(lf *linksFile) {
	s.links = lf
	s.linksMtime, s.linksSize = s.linksStat()
}

// Links returns a copy of the token index, for the daemon's token→session map and serve-time checks.
func (s *Store) Links() (map[string]LinkEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lf, err := s.loadLinks()
	if err != nil {
		return nil, err
	}
	out := make(map[string]LinkEntry, len(lf.Links))
	for k, v := range lf.Links {
		out[k] = v
	}
	return out, nil
}

// SourceStored returns the blob a turn's link resolves to, without touching the original file.
func (s *Store) SourceStored(src string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return "", false
	}
	lf, err := s.loadLinks()
	if err != nil {
		return "", false
	}
	stored, ok := lf.Sources[src]
	if !ok {
		return "", false
	}
	if _, err := os.Lstat(s.Path(stored)); err != nil {
		return "", false
	}
	return stored, true
}

// LinkRecord is everything one published link contributes to links.json. Empty fields are skipped,
// so an inbound attachment passes only the blob and its serving metadata.
type LinkRecord struct {
	Blob        string
	Name        string
	ContentType string
	Source      string      // "<turnSeq>:<abs path>" — which turn's link resolves to this blob
	SeenPath    string      // absolute source path for the identity index
	SeenInfo    os.FileInfo // its stat while it was read; nil leaves the index untouched
}

// Commit applies a whole link — token, turn mapping and content identity — in ONE durable write, and
// returns the blob's stable token. Idempotent: a record that changes nothing writes nothing.
func (s *Store) Commit(r LinkRecord) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return "", ErrRemoved
	}
	lf, err := s.loadLinks()
	if err != nil {
		return "", err
	}
	token := ""
	if e, ok := lf.Links[r.Blob]; ok {
		token = e.Token
	}
	next := lf.clone()
	changed := false
	if token == "" {
		if token, err = newToken(); err != nil {
			return "", err
		}
		next.Links[r.Blob] = LinkEntry{Token: token, Name: r.Name, ContentType: r.ContentType}
		changed = true
	}
	if r.Source != "" && next.Sources[r.Source] != r.Blob {
		next.Sources[r.Source] = r.Blob
		changed = true
	}
	if r.SeenPath != "" && r.SeenInfo != nil {
		if dev, ino, ctime, ok := fileIdentity(r.SeenInfo); ok {
			e := SeenEntry{Blob: r.Blob, Size: r.SeenInfo.Size(), MtimeNS: r.SeenInfo.ModTime().UnixNano(), CtimeNS: ctime, Ino: ino, Dev: dev}
			if cur, had := next.Seen[r.SeenPath]; !had || cur != e {
				next.Seen[r.SeenPath] = e
				changed = true
			}
		}
	}
	if !changed {
		return token, nil
	}
	if err := s.writeLinks(next); err != nil {
		return "", err
	}
	return token, nil
}

// racyWindow: a file written this recently may still change without its stat moving (coarse mtime,
// or a write in progress), so its identity is not trusted.
const racyWindow = 2 * time.Second

// SeenBlob reports the blob this file was last hashed to, if its identity is unchanged. Identity is
// (dev, ino, size, mtime, ctime), the same stat data git's index trusts: an in-place rewrite can
// restore mtime but not ctime, so it is detected.
func (s *Store) SeenBlob(src string, fi os.FileInfo) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return "", false
	}
	lf, err := s.loadLinks()
	if err != nil {
		return "", false
	}
	e, ok := lf.Seen[src]
	if !ok || e.Blob == "" {
		return "", false
	}
	dev, ino, ctime, okID := fileIdentity(fi)
	if !okID || e.Size != fi.Size() || e.MtimeNS != fi.ModTime().UnixNano() || e.CtimeNS != ctime || e.Ino != ino || e.Dev != dev {
		return "", false
	}
	if time.Since(fi.ModTime()) < racyWindow {
		return "", false
	}
	if _, err := os.Lstat(s.Path(e.Blob)); err != nil {
		return "", false
	}
	return e.Blob, true
}

// clone deep-copies the maps so a mutation is persisted before it becomes the cached truth.
func (lf linksFile) clone() linksFile {
	out := linksFile{
		Links:   make(map[string]LinkEntry, len(lf.Links)+1),
		Sources: make(map[string]string, len(lf.Sources)+1),
		Seen:    make(map[string]SeenEntry, len(lf.Seen)+1),
	}
	for k, v := range lf.Links {
		out.Links[k] = v
	}
	for k, v := range lf.Sources {
		out.Sources[k] = v
	}
	for k, v := range lf.Seen {
		out.Seen[k] = v
	}
	return out
}

// linksWrites counts publishes of links.json, so a test can assert write cardinality — the resulting
// file is identical whether it was written once or three times. Atomic: the store lock is per-Store,
// and different sessions publish concurrently.
var linksWrites atomic.Int64

// writeLinks persists lf atomically. Caller holds s.mu.
func (s *Store) writeLinks(lf linksFile) error {
	linksWrites.Add(1)
	if err := mkdirAllSync(s.dir); err != nil {
		return err
	}
	data, err := json.Marshal(lf)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".links-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.linksPath()); err != nil {
		return err
	}
	// The rename is what makes lf the file's contents, so the cache adopts it here; a later
	// fsync failure is reported without reverting the view.
	s.cacheLinks(&lf)
	return fsyncDir(s.dir)
}

// newToken returns a 128-bit random token as 22-char base64url (no padding).
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
