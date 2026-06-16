package main

import "embed"

// spaHTML is the self-contained single-page web UI (vanilla JS/CSS, no external
// deps), served at "/" by the UI server.
//
//go:embed ui_static/index.html
var spaHTML []byte

// emojiFS holds the bundled color-emoji web font (Noto Color Emoji, COLRv1, in
// Google Fonts' unicode-range subsets). Served under /emoji/ and referenced by
// @font-face in the SPA so emoji render identically regardless of the client
// OS's own (possibly outdated) emoji font.
//
//go:embed ui_static/emoji
var emojiFS embed.FS
