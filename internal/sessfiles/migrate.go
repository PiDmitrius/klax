package sessfiles

// MigrateStore converts a data dir from the numeric-session format (sessions keyed by `created`,
// directories <keyDir>/<created>, queue records with `seq`/`session`) to klax_id. It runs before
// the store is loaded and is resumable: the created → klax_id mapping is made durable first in
// sessions.migrate.json, every later step is idempotent, and the journal is renamed to
// sessions.migrated.json only after everything else is durable. Sessions are mapped by position in
// their chat, because legacy `created` values are not unique.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/PiDmitrius/klax/internal/ids"
	"github.com/PiDmitrius/klax/internal/session"
)

type migratedSession struct {
	Created int64  `json:"created"`
	KlaxID  string `json:"klax_id"`
}

type legacySession struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	CWD                    string   `json:"cwd"`
	Created                int64    `json:"created"`
	LastUsed               int64    `json:"last_used"`
	Active                 bool     `json:"active"`
	Backend                string   `json:"backend"`
	Model                  string   `json:"model"`
	ModelOverride          string   `json:"model_override"`
	ThinkOverride          string   `json:"think_override"`
	EffortOverride         string   `json:"effort_override"`
	Sandbox                string   `json:"sandbox"`
	ClaudeTTY              bool     `json:"claude_tty"`
	ContextWindow          int      `json:"ctx_window"`
	ContextUsed            int      `json:"ctx_used"`
	Messages               int      `json:"messages"`
	Groups                 []string `json:"groups"`
	ReadThroughTurn        int64    `json:"read_through_turn"`
	ReadThroughBlock       int      `json:"read_through_block"`
	ReaderReadThroughTurn  int64    `json:"reader_read_through_turn"`
	ReaderReadThroughBlock int      `json:"reader_read_through_block"`
	AppendSystemPrompt     string   `json:"append_system_prompt"`
}

type legacyDefaults struct {
	Backend             string `json:"backend"`
	Model               string `json:"model"`
	Think               string `json:"think"`
	Sandbox             string `json:"sandbox"`
	ClaudeTTY           bool   `json:"claude_tty"`
	CWD                 string `json:"cwd"`
	GroupMode           *bool  `json:"group_mode"`
	GroupVerbose        *bool  `json:"group_verbose"`
	GroupAttachmentMode string `json:"group_attachment_mode"`
	GroupAttachments    *bool  `json:"group_attachments"`
}

type legacyStore struct {
	Chats    map[string]struct{ Sessions []legacySession } `json:"chats"`
	Scope    map[string]legacyDefaults                     `json:"scope_defaults"`
	Sessions []legacySession                               `json:"sessions"`
}

func MigrateStore() error {
	dir := session.StoreDir()
	storePath := filepath.Join(dir, "sessions.json")
	journal := filepath.Join(dir, "sessions.migrate.json")

	data, err := os.ReadFile(storePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	legacy := err == nil && session.IsLegacy(data)
	var old legacyStore
	if legacy {
		if err := json.Unmarshal(data, &old); err != nil {
			return fmt.Errorf("migrate: read sessions.json: %w", err)
		}
		if old.Chats == nil {
			old.Chats = map[string]struct{ Sessions []legacySession }{}
		}
		if len(old.Sessions) > 0 {
			old.Chats["_migrated"] = struct{ Sessions []legacySession }{old.Sessions}
		}
	}

	var mapping map[string][]migratedSession
	jdata, err := os.ReadFile(journal)
	switch {
	case err == nil:
		if err := json.Unmarshal(jdata, &mapping); err != nil {
			return fmt.Errorf("migrate: read %s: %w", journal, err)
		}
	case !os.IsNotExist(err):
		return err
	case !legacy:
		return nil
	default:
		mapping = newMapping(old)
		buf, _ := json.MarshalIndent(mapping, "", "  ")
		if err := writeDurable(journal, buf); err != nil {
			return fmt.Errorf("migrate: write %s: %w", journal, err)
		}
	}

	if err := migrateDirs(mapping); err != nil {
		return err
	}
	if err := migrateQueues(mapping); err != nil {
		return err
	}
	if legacy {
		buf, err := json.MarshalIndent(convertStore(old, mapping), "", "  ")
		if err != nil {
			return err
		}
		if err := writeDurable(storePath, buf); err != nil {
			return fmt.Errorf("migrate: write sessions.json: %w", err)
		}
	}
	if err := os.Rename(journal, filepath.Join(dir, "sessions.migrated.json")); err != nil {
		return err
	}
	return fsyncDir(dir)
}

func newMapping(old legacyStore) map[string][]migratedSession {
	used := map[string]bool{}
	mapping := make(map[string][]migratedSession, len(old.Chats))
	for key, cs := range old.Chats {
		out := make([]migratedSession, len(cs.Sessions))
		for i, sess := range cs.Sessions {
			id := ids.New()
			for used[id] {
				id = ids.New()
			}
			used[id] = true
			out[i] = migratedSession{Created: sess.Created, KlaxID: id}
		}
		mapping[key] = out
	}
	return mapping
}

func sortedKeys(mapping map[string][]migratedSession) []string {
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func migrateDirs(mapping map[string][]migratedSession) error {
	for _, key := range sortedKeys(mapping) {
		parent := WorkDir(key, "")
		moved := false // a directory now lives under its new name: the parent must be durable
		for _, m := range mapping[key] {
			src := filepath.Join(parent, fmt.Sprint(m.Created))
			dst := filepath.Join(parent, m.KlaxID)
			_, srcErr := os.Stat(src)
			_, dstErr := os.Stat(dst)
			switch {
			case srcErr == nil && dstErr == nil:
				return fmt.Errorf("migrate: both %s and %s exist", src, dst)
			case srcErr == nil:
				if err := os.Rename(src, dst); err != nil {
					return err
				}
				moved = true
			case !os.IsNotExist(srcErr):
				return srcErr
			case dstErr == nil:
				moved = true // renamed by an interrupted run whose parent fsync may not have landed
			}
		}
		if moved {
			if err := fsyncDir(parent); err != nil {
				return err
			}
		}
	}
	return nil
}

var queueRenames = map[string]string{"seq": "turn_seq", "session": "backend_id"}

func migrateQueues(mapping map[string][]migratedSession) error {
	for _, key := range sortedKeys(mapping) {
		for _, m := range mapping[key] {
			dir := WorkDir(key, m.KlaxID)
			path := filepath.Join(dir, "queue.jsonl")
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if out, changed := renameQueueKeys(data); changed {
				if err := writeDurable(path, out); err != nil {
					return fmt.Errorf("migrate: rewrite %s: %w", path, err)
				}
			} else if err := fsyncDir(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// renameQueueKeys renames top-level record keys line by line without decoding values (ts is a
// nanosecond timestamp beyond float64 precision). An unreadable line — a torn tail — is kept as is.
func renameQueueKeys(data []byte) ([]byte, bool) {
	var out bytes.Buffer
	changed := false
	lines := bytes.SplitAfter(data, []byte("\n"))
	for _, line := range lines {
		body := bytes.TrimSuffix(line, []byte("\n"))
		var rec map[string]json.RawMessage
		if len(body) == 0 || json.Unmarshal(body, &rec) != nil {
			out.Write(line)
			continue
		}
		hit := false
		for from, to := range queueRenames {
			if v, ok := rec[from]; ok {
				rec[to] = v
				delete(rec, from)
				hit = true
			}
		}
		if !hit {
			out.Write(line)
			continue
		}
		changed = true
		enc, _ := json.Marshal(rec)
		out.Write(enc)
		if len(body) < len(line) {
			out.WriteByte('\n')
		}
	}
	return out.Bytes(), changed
}

func convertStore(old legacyStore, mapping map[string][]migratedSession) any {
	chats := make(map[string]*session.ChatSessions, len(old.Chats))
	for key, cs := range old.Chats {
		out := make([]*session.Session, len(cs.Sessions))
		for i, o := range cs.Sessions {
			think := o.ThinkOverride
			if think == "" {
				think = o.EffortOverride
			}
			out[i] = &session.Session{
				KlaxID: mapping[key][i].KlaxID, BackendID: o.ID, Name: o.Name, CWD: o.CWD,
				LastUsed: o.LastUsed, Active: o.Active, Backend: o.Backend,
				ModelUsed: o.Model, ModelRequested: o.ModelOverride, Think: think, Sandbox: o.Sandbox,
				TTY: o.ClaudeTTY, ContextWindow: o.ContextWindow, ContextUsed: o.ContextUsed,
				Messages: o.Messages, Groups: o.Groups,
				ReadPos:      session.FormatReadPos(o.ReadThroughTurn, o.ReadThroughBlock),
				ReadPosRO:    session.FormatReadPos(o.ReaderReadThroughTurn, o.ReaderReadThroughBlock),
				SystemPrompt: o.AppendSystemPrompt,
			}
		}
		chats[key] = &session.ChatSessions{Sessions: out}
	}
	scope := make(map[string]*session.ScopeDefaults, len(old.Scope))
	for key, o := range old.Scope {
		mode := o.GroupAttachmentMode
		if mode == "" && o.GroupAttachments != nil && *o.GroupAttachments {
			mode = "any"
		}
		scope[key] = &session.ScopeDefaults{
			Backend: o.Backend, ModelRequested: o.Model, Think: o.Think, Sandbox: o.Sandbox,
			TTY: o.ClaudeTTY, CWD: o.CWD, GroupMode: o.GroupMode, GroupVerbose: o.GroupVerbose,
			GroupAttachmentMode: mode,
		}
	}
	return struct {
		Chats map[string]*session.ChatSessions  `json:"chats"`
		Scope map[string]*session.ScopeDefaults `json:"scope_defaults,omitempty"`
	}{chats, scope}
}

// writeDurable replaces path atomically: temp file → fsync → rename → fsync of the directory.
func writeDurable(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".migrate-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0600); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return fsyncDir(dir)
}
