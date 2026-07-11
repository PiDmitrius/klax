package main

import (
	"fmt"
	"mime"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/PiDmitrius/klax/internal/sessfiles"
)

// outLinkRe matches a markdown link or image: optional '!', [label](href) with a
// whitespace-free href.
var outLinkRe = regexp.MustCompile(`(!?)\[([^\]]*)\]\(([^)\s]+)\)`)

// maxOutboundFiles caps how many local files one answer can publish (a budget, not a
// security boundary — confinement does that).
const maxOutboundFiles = 16

// rewriteOutboundForUI scans an agent answer for markdown links/images whose href is
// a LOCAL file inside the session cwd, snapshot-copies
// each into the durable store, mints a durable per-file token and rewrites the href to
// /api/file?ref=. A link that can't be confined or snapshotted degrades to its plain
// label, so the UI never shows a dead local path. UI-only (UI off => no rewrite).
func (d *daemon) rewriteOutboundForUI(sk string, created int64, md string) string {
	if d.uiHub == nil || md == "" || !strings.Contains(md, "](") {
		return md
	}
	// Grab the runner-owned store FIRST (this runs in the run's own Final, so the
	// runner is still live), then check liveness. closeSession deletes the session
	// BEFORE dropping the runner, so a missing session here means a concurrent close
	// won — don't snapshot. Holding the runner-owned store means a close that latches
	// it afterwards makes Adopt return ErrRemoved (degrades to label), never resurrects.
	store := d.sessionStore(sk, created)
	sess := d.store.Get(sk, created)
	if sess == nil {
		return md
	}
	// Pickup root is just the session cwd: the agent writes files where it works and
	// links them in markdown; klax snapshots in-root ones into the durable store.
	roots := []string{sess.CWD}
	n := 0
	return outLinkRe.ReplaceAllStringFunc(md, func(m string) string {
		sub := outLinkRe.FindStringSubmatch(m)
		bang, label, href := sub[1], sub[2], sub[3]
		if isRemoteHref(href) {
			return m // http(s)/data/anchor/already-ours: leave untouched
		}
		if n >= maxOutboundFiles {
			return label
		}
		real, ok := resolveInRoot(href, sess.CWD, roots)
		if !ok {
			return label // outside any root / malformed: degrade to text, never a dead link
		}
		stored, err := store.Adopt(filepath.Base(real), real)
		if err != nil {
			return label
		}
		storedPath := store.Path(stored)
		token, err := d.fileToken(store, sk, created, stored, sessfiles.DisplayName(stored), mime.TypeByExtension(filepath.Ext(stored)))
		if err != nil {
			return label
		}
		n++
		outHref := "/api/file?ref=" + url.QueryEscape(token)
		if bang == "!" {
			if w, h := imageDimensions(storedPath); w > 0 && h > 0 {
				outHref += fmt.Sprintf("&w=%d&h=%d", w, h)
			}
		}
		return bang + "[" + label + "](" + outHref + ")"
	})
}

// isRemoteHref reports hrefs we must not treat as local files: remote schemes, data
// URIs, protocol-relative, anchors, and our own already-minted /api/ URLs.
func isRemoteHref(href string) bool {
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "/api/") {
		return true
	}
	l := strings.ToLower(href)
	for _, p := range []string{"http://", "https://", "data:", "mailto:", "//"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// resolveInRoot decodes href once, resolves it (absolute, or relative to cwd) and
// returns the symlink-resolved real path if it lies inside a root. It rejects
// file://, ~ and control chars; a #fragment is stripped for the local file.
func resolveInRoot(href, cwd string, roots []string) (string, bool) {
	dec, err := url.PathUnescape(href)
	if err != nil {
		dec = href
	}
	if strings.HasPrefix(dec, "~") || strings.HasPrefix(strings.ToLower(dec), "file:") {
		return "", false
	}
	if strings.IndexFunc(dec, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", false
	}
	if i := strings.IndexByte(dec, '#'); i >= 0 {
		dec = dec[:i]
	}
	if dec == "" {
		return "", false
	}
	p := dec
	if !filepath.IsAbs(p) {
		if cwd == "" {
			return "", false
		}
		p = filepath.Join(cwd, p)
	}
	if !pathInRoots(p, roots...) {
		return "", false
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	return real, true
}
