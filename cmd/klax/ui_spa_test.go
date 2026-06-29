package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// serveModule serves an embedded ES module with the right MIME and rejects traversal /
// missing files; handleSPA dispatches *.js / *.css to it without needing auth or a daemon.
func TestServeModule(t *testing.T) {
	s := &uiServer{}

	rec := httptest.NewRecorder()
	s.serveModule(rec, httptest.NewRequest("GET", "/model.js", nil), "/model.js")
	if rec.Code != 200 {
		t.Fatalf("model.js: code %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("model.js content-type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "TurnModel") {
		t.Fatal("model.js body missing TurnModel")
	}

	rec = httptest.NewRecorder()
	s.serveModule(rec, httptest.NewRequest("GET", "/nope.js", nil), "/nope.js")
	if rec.Code != 404 {
		t.Fatalf("missing module should 404, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.serveModule(rec, httptest.NewRequest("GET", "/x", nil), "/sub/x.js")
	if rec.Code != 404 {
		t.Fatalf("path with a slash (traversal) should 404, got %d", rec.Code)
	}

	// handleSPA dispatches a .js path to serveModule (no daemon/auth needed for assets).
	rec = httptest.NewRecorder()
	s.handleSPA(rec, httptest.NewRequest("GET", "/render.js", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "renderModel") {
		t.Fatalf("handleSPA /render.js: code %d", rec.Code)
	}
}
