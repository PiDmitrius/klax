package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/PiDmitrius/klax/internal/pathutil"
)

type systemState struct {
	mu             sync.Mutex
	startedAt      time.Time
	running        bool
	updateStarted  time.Time
	updateFinished time.Time
	lastOK         bool
	lastVersion    string
	releases       []releaseInfo
	checkRunning   bool
	checked        bool
	checkError     string
	updateFn       func(context.Context, systemInstallTarget, io.Writer) updateResult
	releasesFn     func() ([]releaseInfo, error)
}

func newSystemState(started time.Time) *systemState {
	return &systemState{
		startedAt: started,
		updateFn: func(ctx context.Context, target systemInstallTarget, out io.Writer) updateResult {
			if target.Source == "local" {
				return performSourceReinstall(ctx, target.SourceDir, out)
			}
			return performReleaseUpdate(ctx, target.Tag, out)
		},
		releasesFn: fetchReleases,
	}
}

func (d *daemon) systemState() *systemState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.system == nil {
		started := time.Now()
		if d.uiHub != nil {
			started = time.Unix(0, d.uiHub.epoch)
		}
		d.system = newSystemState(started)
	}
	return d.system
}

type systemUpdateView struct {
	Mode       string              `json:"mode"`
	SourceDir  string              `json:"source_dir,omitempty"`
	Running    bool                `json:"running"`
	StartedAt  string              `json:"started_at,omitempty"`
	FinishedAt string              `json:"finished_at,omitempty"`
	OK         bool                `json:"ok"`
	Installed  string              `json:"installed,omitempty"`
	Current    string              `json:"current"`
	Checked    bool                `json:"checked"`
	Checking   bool                `json:"checking"`
	CheckError string              `json:"check_error,omitempty"`
	Releases   []systemReleaseView `json:"releases,omitempty"`
}

type systemReleaseView struct {
	Tag    string `json:"tag"`
	Age    string `json:"age"`
	URL    string `json:"url"`
	Action string `json:"action"`
	Source string `json:"source"`
}

type systemInstallTarget struct {
	Tag       string
	Source    string
	SourceDir string
}

type systemView struct {
	Version   string           `json:"version"`
	StartedAt string           `json:"started_at"`
	UptimeSec int64            `json:"uptime_sec"`
	PID       int              `json:"pid"`
	Platform  string           `json:"platform"`
	Update    systemUpdateView `json:"update"`
}

func (d *daemon) systemView() systemView {
	st := d.systemState()
	st.mu.Lock()
	defer st.mu.Unlock()
	mode := "release"
	if d.cfg.SourceDir != "" {
		mode = "source"
	}
	releases := make([]systemReleaseView, 0, len(st.releases)+1)
	if d.cfg.SourceDir != "" {
		releases = append(releases, systemReleaseView{Tag: "v" + version, Age: "локальная сборка", Action: "reinstall", Source: "local"})
	}
	for _, release := range st.releases {
		releases = append(releases, systemReleaseView{Tag: release.Tag, Age: releaseAge(release.PublishedAt), URL: release.URL, Action: releaseAction(release.Tag), Source: "github"})
	}
	return systemView{
		Version:   version,
		StartedAt: st.startedAt.Format(time.RFC3339),
		UptimeSec: int64(time.Since(st.startedAt).Seconds()),
		PID:       os.Getpid(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Update: systemUpdateView{
			Mode: mode, SourceDir: pathutil.TildePathsInText(d.cfg.SourceDir), Running: st.running,
			StartedAt: formatSystemTime(st.updateStarted), FinishedAt: formatSystemTime(st.updateFinished),
			OK: st.lastOK, Installed: st.lastVersion,
			Current: "v" + version, Checked: st.checked, Checking: st.checkRunning, CheckError: st.checkError, Releases: releases,
		},
	}
}

func releaseAction(tag string) string {
	switch {
	case versionLess(version, tag):
		return "update"
	case versionLess(tag, version):
		return "install"
	default:
		return "reinstall"
	}
}

func formatSystemTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (d *daemon) startUpdateCheck() bool {
	st := d.systemState()
	st.mu.Lock()
	if st.checkRunning {
		st.mu.Unlock()
		return false
	}
	st.checkRunning = true
	st.checked = false
	st.checkError = ""
	st.releases = nil
	fn := st.releasesFn
	st.mu.Unlock()
	go func() {
		releases, err := fn()
		st.mu.Lock()
		defer st.mu.Unlock()
		st.checkRunning = false
		st.checked = true
		if err != nil {
			st.checkError = err.Error()
			return
		}
		if len(releases) > 5 {
			releases = releases[:5]
		}
		st.releases = append([]releaseInfo(nil), releases...)
	}()
	return true
}

func (d *daemon) startSystemUpdate(tag, source string) (bool, string) {
	st := d.systemState()
	st.mu.Lock()
	if st.running {
		st.mu.Unlock()
		return false, "Обновление уже выполняется"
	}
	allowed := source == "local" && d.cfg.SourceDir != "" && tag == "v"+version
	if source == "github" {
		for _, release := range st.releases {
			if release.Tag == tag {
				allowed = true
				break
			}
		}
	}
	if !allowed || (source == "github" && (!st.checked || st.checkError != "")) {
		st.mu.Unlock()
		return false, "Сначала проверьте обновления или выберите версию из списка"
	}
	st.running = true
	st.updateStarted = time.Now()
	st.updateFinished = time.Time{}
	st.lastOK = false
	fn := st.updateFn
	target := systemInstallTarget{Tag: tag, Source: source, SourceDir: d.cfg.SourceDir}
	st.mu.Unlock()

	go func() {
		res := updateResult{}
		func() {
			defer func() {
				if v := recover(); v != nil {
					res = updateResult{Message: fmt.Sprintf("update failed: %v", v)}
				}
			}()
			res = fn(context.Background(), target, io.Discard)
		}()
		if !res.OK {
			res.Message = installFailureMessage(tag, res.Message)
		}
		st.mu.Lock()
		st.updateFinished = time.Now()
		st.lastOK = res.OK
		st.lastVersion = res.Version
		st.mu.Unlock()
		if !res.OK {
			d.uiNotifyAll(res.Message)
		}
		st.mu.Lock()
		st.running = false
		st.mu.Unlock()
	}()
	return true, installStartMessage(tag)
}

func installStartMessage(tag string) string {
	return "Устанавливается klax " + tag + "..."
}

func installFailureMessage(tag, detail string) string {
	message := "Не удалось установить klax " + tag
	if detail != "" {
		message += "\n" + detail
	}
	return message
}

func (d *daemon) systemUpdateRunning() bool {
	d.mu.Lock()
	st := d.system
	d.mu.Unlock()
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.running
}

func (s *uiServer) handleSystem(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(r); !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.d.systemView())
}

func (s *uiServer) handleSystemCheck(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(r); !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	started := s.d.startUpdateCheck()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"checking": true, "started": started})
}

func (s *uiServer) handleSystemUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(r); !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Tag    string `json:"tag"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Tag == "" {
		http.Error(w, "Tag is required", http.StatusBadRequest)
		return
	}
	if body.Source == "" {
		body.Source = "github"
	}
	started, message := s.d.startSystemUpdate(body.Tag, body.Source)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"started": started,
		"running": started || s.d.systemView().Update.Running,
		"message": message,
	})
}
