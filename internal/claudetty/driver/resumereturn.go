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

// ensureResumeReturnDismissed sets resumeReturnDismissed=true in claude's
// global config (~/.claude.json). The flag is what the resume-return nudge's
// own "never ask again" option persists; with it set the nudge — a dialog
// recommending "resume from a summary" when a resumed session is older than
// ~70 minutes and estimated above ~100k tokens — never renders, so the
// driver's blind prompt submission cannot land on the recommended option and
// /compact the session.
//
// Numbers round-trip through json.Number so large literals survive the
// rewrite. A live claude process that loaded the config earlier may rewrite
// the file from memory and drop the flag; that is benign — the next turn
// re-ensures it before spawning. Returns whether the file was modified.
func ensureResumeReturnDismissed(path string) (bool, error) {
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
	if v, ok := cfg["resumeReturnDismissed"].(bool); ok && v {
		return false, nil
	}
	cfg["resumeReturnDismissed"] = true
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
