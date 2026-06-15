package main

import _ "embed"

// spaHTML is the self-contained single-page web UI (vanilla JS/CSS, no external
// deps), served at "/" by the UI server.
//
//go:embed ui_static/index.html
var spaHTML []byte
