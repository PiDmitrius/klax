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
	"github.com/PiDmitrius/klax/internal/transport"
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
printf '%s\n' '{"type":"control_response","response":{"request_id":"models","subtype":"success","response":{"models":[{"value":"default","displayName":"Default","resolvedModel":"new-model[1m]"},{"value":"alias","displayName":"New <Model>","resolvedModel":"new-model[1m]"}]}}}'
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
	if !ok || settings.Model != "opus" || len(settings.Models) != 1 || settings.Models[0].Value != "new-model[1m]" || settings.Models[0].Label != "new-model[1m]" {
		t.Fatal(settings)
	}
	draft := f.d.uiDraftSettings("user:test", f.s.chatID("test"), "claude")
	if len(draft.Models) != 1 || draft.Models[0].Value != settings.Models[0].Value {
		t.Fatal(draft)
	}
	sess := f.d.store.Get("user:test", f.created)
	text := f.d.modelText("user:test", sess)
	if !strings.Contains(text, "new-model[1m]") || strings.Contains(text, "New &lt;Model&gt;") {
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

func TestNoBakedInModelCatalog(t *testing.T) {
	d := newTestDaemon(t)
	d.models = nil
	for _, backend := range []string{"codex", "claude"} {
		settings := d.uiDraftSettings("user:test", "ui:test", backend)
		if len(settings.Models) != 0 {
			t.Fatalf("%s: fabricated model list: %+v", backend, settings.Models)
		}
	}
}

func TestModelUpdateCommand(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			f := newAPIFixture(t, "", "", "")
			f.d.store.UpdateSession("user:test", f.created, func(s *session.Session) { s.Backend = backend; s.ModelOverride = "keep-model" })
			bin := filepath.Join(t.TempDir(), backend)
			script := `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"type":"control_response","response":{"request_id":"models","subtype":"success","response":{"models":[{"resolvedModel":"new-model[1m]"}]}}}'
cat >/dev/null
`
			if backend == "codex" {
				script = `#!/bin/sh
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
printf '%s\n' '{"id":2,"result":{"data":[{"model":"new-model[1m]"}],"nextCursor":null}}'
cat >/dev/null
`
			}
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", filepath.Dir(bin)+":"+os.Getenv("PATH"))
			chatID := f.s.chatID("test")
			tr := &fakeTransport{}
			f.d.transports = map[string]transport.Transport{"ui": tr}
			delivery := newTestDeliveryDaemon(tr)
			f.d.chatEvents = delivery.chatEvents
			f.d.sendPause = delivery.sendPause
			f.d.sendFails = delivery.sendFails
			f.d.handleCommand(chatID, "", "/m_update")
			f.d.drainWg.Wait()
			if len(tr.sendLog) != 1 || !strings.Contains(tr.sendLog[0].text, "new-model[1m]") || !strings.Contains(tr.sendLog[0].text, "/m_update") {
				t.Fatal(tr.sendLog)
			}
			entries := f.d.modelsForBackend(backend)
			if len(entries) != 1 || entries[0].model != "new-model[1m]" || entries[0].alias != "new_model_1m_" {
				t.Fatal(entries)
			}
			sess := f.d.store.Get("user:test", f.created)
			if sess.ModelOverride != "keep-model" {
				t.Fatal(sess.ModelOverride)
			}
			for _, text := range []string{f.d.modelText("user:test", sess), f.d.settingsText(chatID, "user:test", sess)} {
				if !strings.Contains(text, "/m_update") || !strings.Contains(text, "new-model[1m]") {
					t.Fatal(text)
				}
			}
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			f.d.handleCommand(chatID, "", "/m_update")
			f.d.drainWg.Wait()
			if got := f.d.modelsForBackend(backend); len(got) != 1 || got[0] != entries[0] {
				t.Fatal(got)
			}
		})
	}
}

func TestModelEffortsDriveSettingsAndCommands(t *testing.T) {
	for _, backend := range []string{"codex", "claude"} {
		t.Run(backend, func(t *testing.T) {
			f := newAPIFixture(t, "", "", "")
			catalog := map[string][]modelcatalog.Model{backend: {
				{Value: "large", Label: "large", Default: true, Efforts: []string{"low", "high", "ultra"}},
				{Value: "small", Label: "small", Efforts: []string{"high"}},
				{Value: "plain", Label: "plain"},
			}}
			data, err := json.Marshal(catalog)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.dir, "effort-models.json")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			f.d.models, err = modelcatalog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			f.d.store.UpdateSession("user:test", f.created, func(s *session.Session) { s.Backend = backend })
			view, _ := f.d.uiSessionSettings("user:test", f.created)
			if len(view.Efforts) != 3 || view.Efforts[0].Value != "low" {
				t.Fatal(view.Efforts)
			}
			draft := f.d.uiDraftSettings("user:test", f.s.chatID("test"), backend)
			if len(draft.Efforts) != 3 {
				t.Fatal(draft.Efforts)
			}
			chatID := f.s.chatID("test")
			f.d.handleCommand(chatID, "", "/t_ultra")
			if got := f.d.store.Get("user:test", f.created).ThinkOverride; got != "ultra" {
				t.Fatal(got)
			}
			if err = f.d.applyUISessionSettings("user:test", f.created, uiSettingsPatch{Model: str2("small")}); err != nil {
				t.Fatal(err)
			}
			sess := f.d.store.Get("user:test", f.created)
			if sess.ThinkOverride != "" {
				t.Fatal("unsupported effort retained", sess)
			}
			text := f.d.thinkText("user:test", sess)
			if !strings.Contains(text, "/t_high") || strings.Contains(text, "/t_ultra") {
				t.Fatal(text)
			}
			if err = f.d.applyUISessionSettings("user:test", f.created, uiSettingsPatch{Think: str2("ultra")}); err == nil {
				t.Fatal("unsupported effort accepted")
			}
			f.d.handleCommand(chatID, "", "/t_ultra")
			if f.d.store.Get("user:test", f.created).ThinkOverride != "" {
				t.Fatal("messenger accepted unsupported effort")
			}
			f.d.handleCommand(chatID, "", "/t_high")
			f.d.handleCommand(chatID, "", "/model plain")
			sess = f.d.store.Get("user:test", f.created)
			if sess.ThinkOverride != "" {
				t.Fatal("messenger retained unsupported effort")
			}
			view, _ = f.d.uiSessionSettings("user:test", f.created)
			if len(view.Efforts) != 0 {
				t.Fatal(view.Efforts)
			}
			f.d.models = nil
			if len(f.d.effortsForModel(backend, "")) != 0 {
				t.Fatal("fabricated efforts")
			}
		})
	}
}

func setTestModelCatalog(t *testing.T, d *daemon, models []modelcatalog.Model) {
	t.Helper()
	data, err := json.Marshal(map[string][]modelcatalog.Model{"codex": models})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	d.models, err = modelcatalog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
}

func TestModelCommandPreservesCase(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	setTestModelCatalog(t, f.d, []modelcatalog.Model{{Value: "Model-A", Label: "Model-A"}, {Value: "model-a", Label: "model-a"}})
	f.d.handleCommand(f.s.chatID("test"), "", "/m_Model_A")
	got := f.d.store.Get("user:test", f.created).ModelOverride
	if got != "Model-A" {
		t.Fatalf("menu command /m_Model_A selected %q instead of Model-A", got)
	}
}

func TestConcurrentModelEffortSettings(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	setTestModelCatalog(t, f.d, []modelcatalog.Model{{Value: "large", Label: "large", Efforts: []string{"high", "ultra"}}, {Value: "small", Label: "small", Efforts: []string{"high"}}})
	for i := 0; i < 1000; i++ {
		f.d.store.UpdateSession("user:test", f.created, func(s *session.Session) { s.Backend = "codex"; s.ModelOverride = "large"; s.ThinkOverride = "high" })
		start, done := make(chan struct{}), make(chan error, 2)
		go func() {
			<-start
			done <- f.d.applyUISessionSettingsCore("user:test", f.created, uiSettingsPatch{Model: str2("small")})
		}()
		go func() {
			<-start
			done <- f.d.applyUISessionSettingsCore("user:test", f.created, uiSettingsPatch{Think: str2("ultra")})
		}()
		close(start)
		a, b := <-done, <-done
		s := f.d.store.Get("user:test", f.created)
		if s.ModelOverride == "small" && s.ThinkOverride == "ultra" {
			t.Fatalf("unsupported small/ultra saved at iteration %d, request errors: %v / %v", i, a, b)
		}
	}
}
