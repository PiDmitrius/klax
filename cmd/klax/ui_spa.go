package main

import "embed"

// spaHTML is the self-contained single-page web UI (vanilla JS/CSS, no external
// deps), served at "/" by the UI server.
//
//go:embed ui_static/index.html
var spaHTML []byte

// manifestJSON is rendered per request so the installed app uses the same
// configured product name as the SPA instead of maintaining a second title.
//
//go:embed ui_static/manifest.webmanifest
var manifestJSON []byte

// emojiFS holds the bundled color-emoji web font (Noto Color Emoji, COLRv1, in
// Google Fonts' unicode-range subsets). Served under /emoji/ and referenced by
// @font-face in the SPA so emoji render identically regardless of the client
// OS's own (possibly outdated) emoji font.
//
//go:embed ui_static/emoji
var emojiFS embed.FS

// moduleFS holds the SPA's ES modules (model/render/events/compose/… and app.css),
// served at "/<name>.js" / "/<name>.css" by handleSPA. The new client is split into
// these instead of one inline <script>, so a change touches one responsibility.
//
//go:embed ui_static/*.js ui_static/*.css ui_static/*.png
var moduleFS embed.FS
