package main

import (
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/PiDmitrius/klax/internal/sessfiles"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

const fileCacheControl = "private, max-age=86400, immutable"

// inboundText rebuilds a turn's display text from its durable inbound record: the text
// the user actually sent, plus a freshly-minted capability-URL image/link per attached
// file (contract §5/§6 — refs are per-response, never persisted). published reports whether
// every attached file got its link.
func (d *daemon) inboundText(store *sessfiles.Store, t sessfiles.Turn, sk string, klaxID string) (text string, published bool) {
	published = true
	parts := make([]string, 0, 1+len(t.Files))
	if t.Text != "" {
		parts = append(parts, t.Text)
	}
	for _, name := range t.Files {
		ct := mime.TypeByExtension(filepath.Ext(name))
		display := sessfiles.DisplayName(name)
		token, err := d.fileToken(store, sk, klaxID, name, display, ct)
		if err != nil {
			published = false
			continue
		}
		u := "/api/file?ref=" + url.QueryEscape(token)
		label := safeMarkdownLabel(display)
		size := formatFileSize(store.Path(name))
		if strings.HasPrefix(ct, "image/") {
			if w, h := imageDimensions(store.Path(name)); w > 0 && h > 0 {
				u += "&w=" + strconv.Itoa(w) + "&h=" + strconv.Itoa(h)
			}
			parts = append(parts, "!["+label+"]("+u+")")
		} else {
			parts = append(parts, withSize("["+label+"]("+u+")", size))
		}
	}
	return strings.Join(parts, "\n\n"), published
}

func withSize(markdown, size string) string {
	if size == "" {
		return markdown
	}
	return markdown + " (" + size + ")"
}

func formatFileSize(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	n := st.Size()
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	for _, unit := range units {
		v /= 1024
		if v < 1024 {
			return strconv.FormatFloat(v, 'f', 1, 64) + " " + unit
		}
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " PiB"
}

func imageDimensions(path string) (int, int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// safeMarkdownLabel strips markdown-structural characters from a filename so it can't
// break out of a [label](url) / ![label](url) construct and inject markup on reload.
func safeMarkdownLabel(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '[', ']', '(', ')', '!', '\\', '`':
			return -1
		}
		return r
	}, s)
}

// fileToken returns the STABLE durable access token for one stored file (minting + persisting it in
// the session's links.json on first request, reusing it afterwards) and records token -> (session,
// stored file) in the in-memory index so handleFile can resolve it. The token never changes across
// read-model rebuilds, so an attachment's /api/file?ref=… URL — and thus its <img src> — is stable.
func (d *daemon) fileToken(store *sessfiles.Store, sk string, klaxID string, stored, name, contentType string) (string, error) {
	return d.commitLink(store, sk, klaxID, sessfiles.LinkRecord{Blob: stored, Name: name, ContentType: contentType})
}

// commitLink persists a link record and registers its token in the daemon's token→session index.
func (d *daemon) commitLink(store *sessfiles.Store, sk string, klaxID string, r sessfiles.LinkRecord) (string, error) {
	token, err := store.Commit(r)
	if err != nil {
		return "", err
	}
	stored := r.Blob
	d.fileTokensMu.Lock()
	if d.fileTokens == nil {
		d.fileTokens = make(map[string]tokenRef)
	}
	d.fileTokens[token] = tokenRef{sk: sk, klaxID: klaxID, stored: stored}
	d.fileTokensMu.Unlock()
	return token, nil
}

// rebuildFileTokenIndex loads every session's links.json into the in-memory token index at startup, so
// a token minted before the restart still resolves immediately.
func (d *daemon) rebuildFileTokenIndex() {
	type ref struct {
		sk     string
		klaxID string
	}
	var all []ref
	d.store.EachSession(func(sk string, klaxID string) { all = append(all, ref{sk, klaxID}) })
	idx := make(map[string]tokenRef)
	for _, r := range all {
		links, err := d.sessionStore(r.sk, r.klaxID).Links()
		if err != nil {
			continue
		}
		for stored, e := range links {
			if e.Token != "" {
				idx[e.Token] = tokenRef{sk: r.sk, klaxID: r.klaxID, stored: stored}
			}
		}
	}
	d.fileTokensMu.Lock()
	d.fileTokens = idx
	d.fileTokensMu.Unlock()
}

// dropFileTokens forgets a removed session's tokens (its dir — files/ + links.json — is gone, so the
// tokens are already dead; this just bounds the index). Idempotent.
func (d *daemon) dropFileTokens(sk string, klaxID string) {
	d.fileTokensMu.Lock()
	defer d.fileTokensMu.Unlock()
	for token, tr := range d.fileTokens {
		if tr.sk == sk && tr.klaxID == klaxID {
			delete(d.fileTokens, token)
		}
	}
}

// inlineImageTypes are the only media types /api/file serves inline. Everything else
// (HTML, SVG, JS, PDF, unknown) is forced to download as an opaque octet-stream — an
// agent-produced .html/.svg served inline would run JS in the UI origin and could
// read the bearer token from localStorage. SVG is deliberately excluded (it scripts).
var inlineImageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/bmp": true,
}

// handleFile serves a session file by its durable per-file token. The token IS the capability — no
// bearer header is required (an <img src> can't send one). Serving requires, in order: the token
// resolves in the index, the session still exists, the token is still present in that session's
// links.json for the stored file, and the file lies inside the session's files/ dir. One token grants
// access to exactly one file — there is no session-wide key.
func (s *uiServer) handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		apiFail(w, http.StatusMethodNotAllowed, "method-not-allowed", "Метод не поддерживается")
		return
	}
	token := r.URL.Query().Get("ref")
	if token == "" {
		apiFail(w, http.StatusForbidden, "forbidden", "Доступ запрещён")
		return
	}
	s.d.fileTokensMu.Lock()
	tr, ok := s.d.fileTokens[token]
	s.d.fileTokensMu.Unlock()
	if !ok {
		apiFail(w, http.StatusForbidden, "forbidden", "Доступ запрещён")
		return
	}
	// A closed/deleted session's tokens are dead (its dir — files/ + links.json — is gone).
	if s.d.store.Get(tr.sk, tr.klaxID) == nil {
		writeAPIError(w, apiFailure("session-not-found"))
		return
	}
	store := s.d.sessionStore(tr.sk, tr.klaxID)
	// Re-verify the token against links.json (the in-memory index could be stale after a concurrent
	// delete) — it must still map to this stored file.
	links, err := store.Links()
	if err != nil {
		apiFail(w, http.StatusForbidden, "forbidden", "Доступ запрещён")
		return
	}
	entry, ok := links[tr.stored]
	if !ok || entry.Token != token {
		apiFail(w, http.StatusForbidden, "forbidden", "Доступ запрещён")
		return
	}
	// Defense in depth: the stored file must lie inside this session's files/ dir.
	path := store.Path(tr.stored)
	if !pathInRoots(path, filepath.Join(sessfiles.WorkDir(tr.sk, tr.klaxID), "files")) {
		apiFail(w, http.StatusForbidden, "forbidden", "Доступ запрещён")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		apiFail(w, http.StatusNotFound, "file-not-found", "Файл не найден")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		apiFail(w, http.StatusNotFound, "file-not-found", "Файл не найден")
		return
	}
	// Inert serving: only well-known raster images render inline; everything else is
	// downloaded as an opaque octet-stream, never executed as same-origin content.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Stored session files are immutable and their capability URL is stable. Let the user's browser
	// retain them across tab switches; `private` keeps bearer-protected content out of shared caches.
	w.Header().Set("Cache-Control", fileCacheControl)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "sandbox") // blocks active content if opened as a document
	ct := entry.ContentType
	if ct == "" {
		ct = mime.TypeByExtension(filepath.Ext(path))
	}
	mt, _, _ := mime.ParseMediaType(ct)
	if inlineImageTypes[mt] {
		w.Header().Set("Content-Type", mt)
	} else {
		name := entry.Name
		if name == "" {
			name = sessfiles.DisplayName(filepath.Base(path))
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	}
	http.ServeContent(&jsonErrorWriter{ResponseWriter: w}, r, filepath.Base(path), fi.ModTime(), f)
}

// jsonErrorWriter turns the plain-text errors http.ServeContent writes (an unsatisfiable Range)
// into the API's JSON error.
type jsonErrorWriter struct {
	http.ResponseWriter
	failed bool
}

func (w *jsonErrorWriter) WriteHeader(status int) {
	if status < http.StatusBadRequest {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.failed = true
	h := w.ResponseWriter.Header()
	h.Del("Content-Disposition")
	h.Del("Content-Range")
	if status == http.StatusRequestedRangeNotSatisfiable {
		apiFail(w.ResponseWriter, status, "bad-range", "Запрошенный диапазон недоступен")
	} else {
		apiFail(w.ResponseWriter, status, "bad-request", "Некорректный запрос файла")
	}
}

func (w *jsonErrorWriter) Write(b []byte) (int, error) {
	if w.failed {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// pathInRoots reports whether path (after symlink resolution) lies inside any of the
// given roots — the containment check behind every served ref. A non-existent path,
// a symlink escaping a root, or a traversal all resolve out and return false.
func pathInRoots(path string, roots ...string) bool {
	rp, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rr, rp)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
