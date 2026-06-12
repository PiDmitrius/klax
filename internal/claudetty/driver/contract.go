package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ensureClaudeContract pins the claude global-config keys the tty path
// assumes as its operating contract, so no startup dialog can steal the
// typed prompt's keystrokes:
//
//   - resumeReturnDismissed: the resume-return nudge recommends "resume from
//     a summary" for sessions older than ~70 minutes above ~100k estimated
//     tokens, and confirming it submits a literal /compact (observed as
//     trigger:"manual" boundaries at 186k–212k tokens). The flag is what the
//     dialog's own "never ask again" option persists.
//   - hasCompletedOnboarding: the first-run wizard would wedge a fresh
//     host's first turn waiting for a human. (Credentials stay the
//     operator's job; this only skips the cosmetic setup flow.)
//   - projects[cwd].hasTrustDialogAccepted: the workspace-trust dialog
//     renders on every spawn in an untrusted directory. The operator chose
//     the directory by configuring the session, so it is trusted by
//     definition; the in-loop dialog detection stays as a fallback.
//
// Numbers round-trip through json.Number so large literals survive the
// rewrite. A live claude process that loaded the config earlier may rewrite
// the file from memory and drop the keys; that is benign — the next turn
// re-ensures them before its claude reads the file. Returns whether the
// file was modified.
func ensureClaudeContract(path, cwd string) (bool, error) {
	cfg := map[string]any{}
	mode := fs.FileMode(0600)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// First run on this host; claude merges its own defaults later.
	case err != nil:
		return false, err
	default:
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&cfg); err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	changed := false
	for _, k := range []string{"resumeReturnDismissed", "hasCompletedOnboarding"} {
		if v, ok := cfg[k].(bool); !ok || !v {
			cfg[k] = true
			changed = true
		}
	}
	if cwd != "" {
		proj, err := projectEntry(cfg, filepath.Clean(cwd))
		if err != nil {
			return false, fmt.Errorf("config %s: %w", path, err)
		}
		if v, ok := proj["hasTrustDialogAccepted"].(bool); !ok || !v {
			proj["hasTrustDialogAccepted"] = true
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

// projectEntry returns the projects[cwd] object, creating missing levels.
// A present-but-non-object level is an error: better to skip the contract
// (the caller treats it as best effort) than to clobber an unexpected shape.
func projectEntry(cfg map[string]any, cwd string) (map[string]any, error) {
	projects, ok := cfg["projects"].(map[string]any)
	if !ok {
		if v, exists := cfg["projects"]; exists && v != nil {
			return nil, fmt.Errorf("projects is not an object")
		}
		projects = map[string]any{}
		cfg["projects"] = projects
	}
	proj, ok := projects[cwd].(map[string]any)
	if !ok {
		if v, exists := projects[cwd]; exists && v != nil {
			return nil, fmt.Errorf("projects[%s] is not an object", cwd)
		}
		proj = map[string]any{}
		projects[cwd] = proj
	}
	return proj, nil
}

// withBypassAccepted injects skipDangerousModePermissionPrompt into the
// --settings payload when launching in bypassPermissions mode. The bypass
// warning dialog defaults to "No, exit" — a blind Enter on a host whose user
// settings never accepted it would kill the session. Flag settings are
// scoped to this process, so no user file is touched; klax only passes
// bypassPermissions when its own /sandbox setting is off, i.e. the operator
// already made this call.
func withBypassAccepted(settingsJSON, permissionMode string) string {
	if permissionMode != "bypassPermissions" {
		return settingsJSON
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(settingsJSON), &m); err != nil || m == nil {
		return settingsJSON
	}
	m["skipDangerousModePermissionPrompt"] = true
	out, err := json.Marshal(m)
	if err != nil {
		return settingsJSON
	}
	return string(out)
}
