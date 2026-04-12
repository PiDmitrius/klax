// Package session manages AI coding sessions.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type ScopeDefaults struct {
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	Think   string `json:"think,omitempty"`
}

type Session struct {
	ID                 string `json:"id"`                       // session UUID (claude or codex thread_id)
	Name               string `json:"name"`                     // user-friendly name
	CWD                string `json:"cwd"`                      // working directory
	Created            int64  `json:"created"`                  // unix timestamp
	LastUsed           int64  `json:"last_used"`                // unix timestamp
	Active             bool   `json:"active"`                   // currently selected
	Backend            string `json:"backend,omitempty"`        // "claude" (default) or "codex"
	Model              string `json:"model,omitempty"`          // last used model (from result)
	ModelOverride      string `json:"model_override,omitempty"` // user-selected model
	ThinkOverride      string `json:"think_override,omitempty"` // thinking level
	ContextWindow      int    `json:"ctx_window,omitempty"`
	ContextUsed        int    `json:"ctx_used,omitempty"`
	Messages           int    `json:"messages"` // user message count
	PermissionMode     string `json:"permission_mode,omitempty"`
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"`
	// Deprecated: rate limits moved to global config per backend.
	// Keep fields for JSON backward compat (old sessions.json).
	RateLimitStatus  string `json:"rl_status,omitempty"`
	RateLimitResets  int64  `json:"rl_resets,omitempty"`
	RateLimitType    string `json:"rl_type,omitempty"`
	RateLimitOverage bool   `json:"rl_overage,omitempty"`
}

type ChatSessions struct {
	Sessions []*Session `json:"sessions"`
}

type Store struct {
	mu    sync.Mutex
	Chats map[string]*ChatSessions  `json:"chats"`
	Scope map[string]*ScopeDefaults `json:"scope_defaults,omitempty"`
	path  string
}

func (s *Session) UnmarshalJSON(data []byte) error {
	type alias Session
	var payload struct {
		alias
		LegacyEffortOverride string `json:"effort_override,omitempty"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	*s = Session(payload.alias)
	if s.ThinkOverride == "" {
		s.ThinkOverride = payload.LegacyEffortOverride
	}
	return nil
}

func cloneSession(sess *Session) *Session {
	if sess == nil {
		return nil
	}
	cp := *sess
	return &cp
}

func cloneDefaults(def *ScopeDefaults) *ScopeDefaults {
	if def == nil {
		return nil
	}
	cp := *def
	return &cp
}

func cloneSessions(sessions []*Session) []*Session {
	if len(sessions) == 0 {
		return nil
	}
	out := make([]*Session, len(sessions))
	for i, sess := range sessions {
		out[i] = cloneSession(sess)
	}
	return out
}

func StoreDir() string {
	if d := os.Getenv("KLAX_DATA_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "klax")
}

func LoadStore() (*Store, error) {
	path := filepath.Join(StoreDir(), "sessions.json")
	s := &Store{path: path, Chats: make(map[string]*ChatSessions), Scope: make(map[string]*ScopeDefaults)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}

	// Try new format first.
	if err := json.Unmarshal(data, s); err == nil && (len(s.Chats) > 0 || len(s.Scope) > 0) {
		s.normalize()
		return s, nil
	}

	// Fall back to legacy flat format.
	s.Chats = make(map[string]*ChatSessions)
	s.Scope = make(map[string]*ScopeDefaults)
	var legacy struct {
		Sessions []*Session `json:"sessions"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, err
	}
	if len(legacy.Sessions) > 0 {
		s.Chats["_migrated"] = &ChatSessions{Sessions: legacy.Sessions}
	}
	s.normalize()
	return s, nil
}

// MigrateTo moves legacy sessions to the given chatID.
func (s *Store) MigrateTo(chatID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	migrated, ok := s.Chats["_migrated"]
	if !ok {
		return false
	}
	s.Chats[chatID] = migrated
	delete(s.Chats, "_migrated")
	return true
}

// MergeKeys merges sessions from oldKeys into targetKey.
// Sessions from old keys are appended to the target; old keys are deleted.
// Returns true if any keys were merged.
func (s *Store) MergeKeys(targetKey string, oldKeys []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := false
	target := s.chat(targetKey)
	for _, old := range oldKeys {
		cs, ok := s.Chats[old]
		if !ok || old == targetKey {
			continue
		}
		target.Sessions = append(target.Sessions, cs.Sessions...)
		delete(s.Chats, old)
		merged = true
	}
	// Ensure at most one session is active.
	if merged {
		foundActive := false
		for i := len(target.Sessions) - 1; i >= 0; i-- {
			if target.Sessions[i].Active {
				if foundActive {
					target.Sessions[i].Active = false
				}
				foundActive = true
			}
		}
	}
	return merged
}

func (s *Store) Save() error {
	s.mu.Lock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		s.mu.Unlock()
		return err
	}
	path := s.path
	payload := struct {
		Chats map[string]*ChatSessions  `json:"chats"`
		Scope map[string]*ScopeDefaults `json:"scope_defaults,omitempty"`
	}{
		Chats: make(map[string]*ChatSessions, len(s.Chats)),
		Scope: make(map[string]*ScopeDefaults, len(s.Scope)),
	}
	for key, chat := range s.Chats {
		payload.Chats[key] = &ChatSessions{Sessions: cloneSessions(chat.Sessions)}
	}
	for key, def := range s.Scope {
		payload.Scope[key] = cloneDefaults(def)
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func (s *Store) chat(chatID string) *ChatSessions {
	cs, ok := s.Chats[chatID]
	if !ok {
		cs = &ChatSessions{}
		s.Chats[chatID] = cs
	}
	return cs
}

func (s *Store) scope(chatID string) *ScopeDefaults {
	def, ok := s.Scope[chatID]
	if !ok {
		def = &ScopeDefaults{}
		s.Scope[chatID] = def
	}
	return def
}

func (s *Store) normalize() {
	if s.Chats == nil {
		s.Chats = make(map[string]*ChatSessions)
	}
	if s.Scope == nil {
		s.Scope = make(map[string]*ScopeDefaults)
	}
	for key, chat := range s.Chats {
		if chat == nil {
			s.Chats[key] = &ChatSessions{}
			continue
		}
		if chat.Sessions == nil {
			chat.Sessions = []*Session{}
		}
		def := s.scope(key)
		for _, sess := range chat.Sessions {
			if sess == nil {
				continue
			}
			if sess.Backend == "" && sess.Messages > 0 {
				sess.Backend = "claude"
			}
			if def.Backend == "" && sess.Backend != "" {
				def.Backend = sess.Backend
			}
		}
	}
}

func (s *Store) SessionsFor(chatID string) []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSessions(s.chat(chatID).Sessions)
}

func (s *Store) Active(chatID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.chat(chatID).Sessions {
		if sess.Active {
			return cloneSession(sess)
		}
	}
	return nil
}

func (s *Store) ScopeDefaults(chatID string) *ScopeDefaults {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneDefaults(s.scope(chatID))
}

func (s *Store) EnsureScopeDefaults(chatID string, fallback ScopeDefaults) *ScopeDefaults {
	s.mu.Lock()
	defer s.mu.Unlock()
	def := s.scope(chatID)
	if def.Backend == "" {
		def.Backend = fallback.Backend
	}
	return cloneDefaults(def)
}

func (s *Store) UpdateScopeDefaults(chatID string, fn func(*ScopeDefaults)) *ScopeDefaults {
	s.mu.Lock()
	defer s.mu.Unlock()
	def := s.scope(chatID)
	fn(def)
	return cloneDefaults(def)
}

func (s *Store) UpdateActive(chatID string, fn func(*Session)) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.chat(chatID).Sessions {
		if sess.Active {
			fn(sess)
			return cloneSession(sess)
		}
	}
	return nil
}

func (s *Store) UpdateSession(chatID string, created int64, fn func(*Session)) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.chat(chatID).Sessions {
		if sess.Created == created {
			fn(sess)
			return cloneSession(sess)
		}
	}
	return nil
}

func (s *Store) Ensure(chatID, name, cwd string, defaults ScopeDefaults) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.chat(chatID)
	def := s.scope(chatID)
	if def.Backend == "" {
		def.Backend = defaults.Backend
	}
	for _, sess := range cs.Sessions {
		if sess.Active {
			if cwd != "" && sess.CWD != cwd {
				sess.CWD = cwd
			}
			return cloneSession(sess)
		}
	}
	for _, sess := range cs.Sessions {
		sess.Active = false
	}
	sess := &Session{
		Name:          name,
		CWD:           cwd,
		Created:       time.Now().Unix(),
		Active:        true,
		Backend:       def.Backend,
		ModelOverride: def.Model,
		ThinkOverride: def.Think,
	}
	cs.Sessions = append(cs.Sessions, sess)
	return cloneSession(sess)
}

func (s *Store) New(chatID, name, cwd string, defaults ScopeDefaults) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.chat(chatID)
	def := s.scope(chatID)
	if def.Backend == "" {
		def.Backend = defaults.Backend
	}
	for _, sess := range cs.Sessions {
		sess.Active = false
	}
	sess := &Session{
		Name:          name,
		CWD:           cwd,
		Created:       time.Now().Unix(),
		Active:        true,
		Backend:       def.Backend,
		ModelOverride: def.Model,
		ThinkOverride: def.Think,
	}
	cs.Sessions = append(cs.Sessions, sess)
	return cloneSession(sess)
}

func (s *Store) Delete(chatID string, idx int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.chat(chatID)
	if idx < 0 || idx >= len(cs.Sessions) {
		return false
	}
	cs.Sessions = append(cs.Sessions[:idx], cs.Sessions[idx+1:]...)
	return true
}

func (s *Store) Switch(chatID string, idx int) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.chat(chatID)
	if idx < 0 || idx >= len(cs.Sessions) {
		return nil
	}
	for _, sess := range cs.Sessions {
		sess.Active = false
	}
	cs.Sessions[idx].Active = true
	return cloneSession(cs.Sessions[idx])
}
