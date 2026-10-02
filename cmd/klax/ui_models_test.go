package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PiDmitrius/klax/internal/modelcatalog"
	"github.com/PiDmitrius/klax/internal/session"
)

func TestModelRefreshFeedsAllSelectorsAndKeepsSelection(t *testing.T) {
	f := readerFixture(t)
	var err error
	f.d.models, err = modelcatalog.Open(filepath.Join(f.dir, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The fake accepts control initialization only; a generation request fails the exchange.
	bin := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
IFS= read -r line
case "$line" in *'"subtype":"initialize"'*) ;; *) exit 1 ;; esac
printf '%s\n' '{"type":"control_response","response":{"request_id":"models","subtype":"success","response":{"models":[{"value":"default","displayName":"Default"},{"value":"new-model[1m]","displayName":"New <Model>"}]}}}'
cat >/dev/null
`
	if err = os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(bin)+":"+os.Getenv("PATH"))
	f.d.store.UpdateSession("user:test", f.created, func(s *session.Session) { s.Backend = "claude"; s.ModelOverride = "opus" })
	w := f.request("/api/models/refresh", `{"backend":"claude"}`, "access")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct{ Models []modelcatalog.Model }
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Models) != 1 {
		t.Fatal(err, w.Body.String())
	}
	settings, ok := f.d.uiSessionSettings("user:test", f.created)
	if !ok || settings.Model != "opus" || len(settings.Models) != 1 || settings.Models[0].Value != "new-model[1m]" {
		t.Fatal(settings)
	}
	draft := f.d.uiDraftSettings("user:test", f.s.chatID("test"), "claude")
	if len(draft.Models) != 1 || draft.Models[0].Value != settings.Models[0].Value {
		t.Fatal(draft)
	}
	sess := f.d.store.Get("user:test", f.created)
	text := f.d.modelText("user:test", sess)
	if !strings.Contains(text, "New &lt;Model&gt;") {
		t.Fatal(text)
	}
	if _, err = f.d.validateSettingsPatch(sess, "claude", false, uiSettingsPatch{Model: str2("opus")}); err != nil {
		t.Fatal("existing choice rejected", err)
	}
	if _, err = f.d.validateSettingsPatch(sess, "claude", false, uiSettingsPatch{Model: str2("new-model[1m]")}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.d.validateSettingsPatch(sess, "codex", false, uiSettingsPatch{Model: str2("new-model[1m]")}); err == nil {
		t.Fatal("backend catalogs mixed")
	}
	selected := f.d.modelsForBackend("claude")[0]
	if !strings.Contains(text, "/m_"+selected.alias) {
		t.Fatal("messenger uses another catalog")
	}
	f.d.models, err = modelcatalog.Open(filepath.Join(f.dir, "models.json"))
	if err != nil || f.d.modelsForBackend("claude")[0] != selected {
		t.Fatal("restart changed catalog", err)
	}
	for _, tc := range []struct {
		body, token string
		status      int
	}{
		{`{"backend":"other"}`, "access", 400},
		{`{"backend":"claude"}`, "reader", 403},
		{`{"backend":"claude"}`, "invalid", 401},
	} {
		if w := f.request("/api/models/refresh", tc.body, tc.token); w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// Refreshing the catalog alone must not change the selected model on disk or in memory.
	if got := f.d.store.Get("user:test", f.created).ModelOverride; got != "opus" {
		t.Fatal(got)
	}
	patch := fmt.Sprintf(`{"session":%d,"model":"new-model[1m]"}`, f.created)
	if w := f.request("/api/settings", patch, "access"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
