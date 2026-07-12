package sessfiles

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
)

// LinkEntry is one durable per-file access token: a stable random token addressing a SINGLE stored
// file, plus its display name and content type for serving. Persisted in links.json (keyed by the
// stored filename) so the token never changes across read-model rebuilds or restarts — unlike an
// ephemeral capability seal, whose value changed every mint and made an attachment's <img src> churn.
type LinkEntry struct {
	Token       string `json:"token"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
}

type linksFile struct {
	Links map[string]LinkEntry `json:"links"` // keyed by STORED filename (e.g. "000001-01-image.png")
}

func (s *Store) linksPath() string { return filepath.Join(s.dir, "links.json") }

// loadLinks reads links.json (caller holds s.mu). A missing/empty file yields an empty map.
func (s *Store) loadLinks() (linksFile, error) {
	lf := linksFile{Links: map[string]LinkEntry{}}
	data, err := os.ReadFile(s.linksPath())
	if os.IsNotExist(err) {
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
	return lf, nil
}

// Links returns a snapshot of every link entry (keyed by stored filename) — used to rebuild the
// daemon's token→session index at startup and to verify a token at serve time.
func (s *Store) Links() (map[string]LinkEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lf, err := s.loadLinks()
	if err != nil {
		return nil, err
	}
	return lf.Links, nil
}

// EnsureLink returns the STABLE access token for one stored file: it mints and durably persists a
// fresh 128-bit token on first request and returns the existing token on every rebuild afterwards.
// name/contentType are recorded for serving. Idempotent; safe across rebuilds and restarts.
func (s *Store) EnsureLink(stored, name, contentType string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return "", ErrRemoved
	}
	lf, err := s.loadLinks()
	if err != nil {
		return "", err
	}
	if e, ok := lf.Links[stored]; ok && e.Token != "" {
		return e.Token, nil // stable — never re-minted
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	lf.Links[stored] = LinkEntry{Token: token, Name: name, ContentType: contentType}
	if err := s.writeLinks(lf); err != nil {
		return "", err
	}
	return token, nil
}

// writeLinks persists lf atomically and durably (temp → fsync → rename → fsync dir). Caller holds s.mu.
func (s *Store) writeLinks(lf linksFile) error {
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
	defer os.Remove(tmpName) // cleans temp on error and after a successful rename
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
