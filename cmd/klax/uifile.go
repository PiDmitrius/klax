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
	"time"

	"github.com/PiDmitrius/klax/internal/sealref"
	"github.com/PiDmitrius/klax/internal/sessfiles"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// inboundText rebuilds a turn's display text from its durable inbound record: the text
// the user actually sent, plus a freshly-minted capability-URL image/link per attached
// file (contract §5/§6 — refs are per-response, never persisted).
func (d *daemon) inboundText(store *sessfiles.Store, t sessfiles.Turn, sk string, created int64) string {
	parts := make([]string, 0, 1+len(t.Files))
	if t.Text != "" {
		parts = append(parts, t.Text)
	}
	for _, name := range t.Files {
		ct := mime.TypeByExtension(filepath.Ext(name))
		ref, err := d.mintFileRef(sk, created, store.Path(name), ct)
		if err != nil {
			continue
		}
		u := "/api/file?ref=" + url.QueryEscape(ref)
		label := safeMarkdownLabel(sessfiles.DisplayName(name))
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
	return strings.Join(parts, "\n\n")
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

// fileRefTTL bounds how long a minted file capability URL stays valid. fileRefRemintGrace re-mints a
// cached ref once it is within this window of expiry, so a served <img src> stays valid.
const (
	fileRefTTL         = time.Hour
	fileRefRemintGrace = 10 * time.Minute
)

// inlineImageTypes are the only media types /api/file serves inline. Everything else
// (HTML, SVG, JS, PDF, unknown) is forced to download as an opaque octet-stream — an
// agent-produced .html/.svg served inline would run JS in the UI origin and could
// read the bearer token from localStorage. SVG is deliberately excluded (it scripts).
var inlineImageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/bmp": true,
}

// mintFileRef returns a capability URL ref for one of a session's files. It CACHES the sealed ref per
// (session, path, contentType) and returns the SAME ref across read-model rebuilds until it nears
// expiry — the sealed ref is randomized per mint (fresh nonce), so re-minting on every rebuild changed
// an attachment's URL and made its <img> re-decode/flicker. A cached ref reuses its full TTL of
// validity (no security change: a single mint was always valid for the whole TTL). Callers only ever
// mint for paths already confined to a session root, so a leaked UI token can't widen this to
// arbitrary read.
func (d *daemon) mintFileRef(sk string, created int64, path, contentType string) (string, error) {
	key := sk + "\x00" + strconv.FormatInt(created, 10) + "\x00" + path + "\x00" + contentType
	now := time.Now()
	d.fileRefsMu.Lock()
	if e, ok := d.fileRefs[key]; ok && e.exp-now.Unix() > int64(fileRefRemintGrace.Seconds()) {
		d.fileRefsMu.Unlock()
		return e.ref, nil // stable across rebuilds → same <img src>, no re-decode
	}
	d.fileRefsMu.Unlock()

	exp := now.Add(fileRefTTL)
	ref, err := d.sealer.Seal(sealref.Payload{
		SessionKey:  sk,
		Created:     created,
		Path:        path,
		ContentType: contentType,
		Exp:         exp.Unix(),
	})
	if err != nil {
		return "", err
	}
	d.fileRefsMu.Lock()
	if d.fileRefs == nil {
		d.fileRefs = make(map[string]fileRefEntry)
	}
	d.fileRefs[key] = fileRefEntry{ref: ref, exp: exp.Unix()}
	for k, e := range d.fileRefs { // opportunistic sweep of expired entries (bounds the map)
		if e.exp <= now.Unix() {
			delete(d.fileRefs, k)
		}
	}
	d.fileRefsMu.Unlock()
	return ref, nil
}

// handleFile serves a session file by sealed ref. The ref IS the capability — no
// bearer header is required (an <img src> can't send one). The ref proves the
// server minted it for this session+path+exp; serving additionally requires the
// session to still exist and the path to remain inside a session root.
func (s *uiServer) handleFile(w http.ResponseWriter, r *http.Request) {
	p, err := s.d.sealer.Open(r.URL.Query().Get("ref"), time.Now())
	if err != nil {
		http.Error(w, "invalid reference", http.StatusForbidden)
		return
	}
	// Liveness: a closed/deleted session's refs are dead even if bytes linger until
	// the TTL sweep.
	sess := s.d.store.Get(p.SessionKey, p.Created)
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	// Defense in depth: re-confine to a session root at serve time (minting already
	// checked, but the path could have changed).
	roots := []string{sess.CWD, filepath.Join(sessfiles.WorkDir(p.SessionKey, p.Created), "files")}
	if !pathInRoots(p.Path, roots...) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	f, err := os.Open(p.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	// Inert serving: only well-known raster images render inline; everything else is
	// downloaded as an opaque octet-stream, never executed as same-origin content.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "sandbox") // blocks active content if opened as a document
	ct := p.ContentType
	if ct == "" {
		ct = mime.TypeByExtension(filepath.Ext(p.Path))
	}
	mt, _, _ := mime.ParseMediaType(ct)
	if inlineImageTypes[mt] {
		w.Header().Set("Content-Type", mt)
	} else {
		name := sessfiles.DisplayName(filepath.Base(p.Path))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	}
	http.ServeContent(w, r, filepath.Base(p.Path), fi.ModTime(), f)
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
