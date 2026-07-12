package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PiDmitrius/klax/internal/config"
)

func systemTestServer() (*uiServer, *systemState) {
	st := newSystemState(time.Now().Add(-time.Minute))
	d := &daemon{cfg: &config.Config{SourceDir: "/source"}, uiHub: newUIHub(), system: st}
	return &uiServer{d: d, tokens: map[string]string{"token": "owner"}}, st
}

func authSystemRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer token")
	return r
}

func authSystemJSON(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer token")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestSystemAPIAuthAndView(t *testing.T) {
	s, _ := systemTestServer()
	h := s.routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/system", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authSystemRequest(http.MethodGet, "/api/system"))
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d", rec.Code)
	}
	var got systemView
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version != version || got.Update.Mode != "source" || got.Update.SourceDir != "/source" || got.Update.Checked || len(got.Update.Releases) != 1 {
		t.Fatalf("unexpected view: %+v", got)
	}
	if local := got.Update.Releases[0]; local.Source != "local" || local.Tag != "v"+version {
		t.Fatalf("initial local artifact = %+v", local)
	}
}

func TestSystemCheckIsExplicit(t *testing.T) {
	s, st := systemTestServer()
	called := make(chan struct{}, 1)
	st.releasesFn = func() ([]releaseInfo, error) { called <- struct{}{}; return []releaseInfo{{Tag: "v9.9.9"}}, nil }
	h := s.routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authSystemRequest(http.MethodGet, "/api/system"))
	select {
	case <-called:
		t.Fatal("GET /api/system performed an implicit external update check")
	default:
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authSystemRequest(http.MethodPost, "/api/system/check"))
	if rec.Code != http.StatusOK {
		t.Fatalf("check status = %d", rec.Code)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("explicit check did not call latest release lookup")
	}
}

func TestSystemCheckedViewIncludesLocalArtifact(t *testing.T) {
	s, st := systemTestServer()
	st.checked = true
	st.releases = []releaseInfo{{Tag: "v9.9.9", PublishedAt: time.Now().Format(time.RFC3339)}}
	got := s.d.systemView().Update.Releases
	if len(got) != 2 {
		t.Fatalf("release choices = %+v", got)
	}
	local := got[0]
	if local.Tag != "v"+version || local.Source != "local" || local.Action != "reinstall" || local.Age != "локальная сборка" {
		t.Fatalf("local artifact = %+v", local)
	}
}

func TestSystemUpdateMethodAndSingleFlight(t *testing.T) {
	s, st := systemTestServer()
	st.checked, st.releases = true, []releaseInfo{{Tag: "v9.9.9"}}
	started := make(chan struct{})
	release := make(chan struct{})
	st.updateFn = func(context.Context, systemInstallTarget, io.Writer) updateResult {
		close(started)
		<-release
		return updateResult{OK: true, Version: "9.9.9", Message: "done"}
	}
	h := s.routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authSystemRequest(http.MethodGet, "/api/system/update"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET update status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authSystemJSON(http.MethodPost, "/api/system/update", `{"tag":"v8.8.8"}`))
	var rejected map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&rejected); err != nil {
		t.Fatal(err)
	}
	if rejected["running"] != false {
		t.Fatalf("unchecked tag accepted: %#v", rejected)
	}
	if rejected["started"] != false {
		t.Fatalf("unchecked tag reported started: %#v", rejected)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authSystemJSON(http.MethodPost, "/api/system/update", `{"tag":"v9.9.9"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("first update status = %d", rec.Code)
	}
	var first map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first["started"] != true {
		t.Fatalf("first update not reported started: %#v", first)
	}
	<-started

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, authSystemJSON(http.MethodPost, "/api/system/update", `{"tag":"v9.9.9"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("second update status = %d", rec.Code)
	}
	var second map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second["message"] != "Обновление уже выполняется" {
		t.Fatalf("second response = %#v", second)
	}
	if second["started"] != false {
		t.Fatalf("second update reported started: %#v", second)
	}

	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !s.d.systemView().Update.Running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	got := s.d.systemView().Update
	if got.Running || !got.OK || got.Installed != "9.9.9" {
		t.Fatalf("final update = %+v", got)
	}
}

func TestSystemVersionActions(t *testing.T) {
	for _, tc := range []struct{ latest, action string }{
		{"v0.0.1", "install"},
		{"v" + version, "reinstall"},
		{"v99.0.0", "update"},
	} {
		if got := releaseAction(tc.latest); got != tc.action {
			t.Fatalf("latest %s: action %s, want %s", tc.latest, got, tc.action)
		}
	}
}

func TestInstallStatusMessages(t *testing.T) {
	if got := installStartMessage("v2.0.0"); got != "Устанавливается klax v2.0.0..." {
		t.Fatalf("start = %q", got)
	}
	if got := installFailureMessage("v2.0.0", "disk full"); got != "Не удалось установить klax v2.0.0\ndisk full" {
		t.Fatalf("failure = %q", got)
	}
}

func TestStartupNotice(t *testing.T) {
	for _, tc := range []struct {
		marker *restartMarker
		kind   string
		text   string
	}{
		{&restartMarker{Reason: "update"}, "installed", "✅ Установлен klax v" + version},
		{&restartMarker{Reason: "signal"}, "started", "✅ Запущен klax v" + version},
		{nil, "started", "✅ Запущен klax v" + version},
	} {
		kind, text := startupNotice(tc.marker)
		if kind != tc.kind || text != tc.text {
			t.Fatalf("startupNotice(%+v) = %q, %q", tc.marker, kind, text)
		}
	}
}

func TestSystemLocalReinstallUsesSourceArtifact(t *testing.T) {
	s, st := systemTestServer()
	targets := make(chan systemInstallTarget, 1)
	st.updateFn = func(_ context.Context, target systemInstallTarget, _ io.Writer) updateResult {
		targets <- target
		return updateResult{OK: true, Version: version}
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, authSystemJSON(http.MethodPost, "/api/system/update", `{"tag":"v`+version+`","source":"local"}`))
	var response map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["started"] != true {
		t.Fatalf("local reinstall response = %#v", response)
	}
	select {
	case target := <-targets:
		if target.Source != "local" || target.Tag != "v"+version || target.SourceDir != "/source" {
			t.Fatalf("local target = %+v", target)
		}
	case <-time.After(time.Second):
		t.Fatal("local reinstall did not start")
	}
}
