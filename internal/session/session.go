// Package session manages AI coding sessions.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Session struct {
	ID                 string `json:"id"`                       // session UUID (claude or codex thread_id)
	Name               string `json:"name"`                     // user-friendly name
	CWD                string `json:"cwd"`                      // working directory
	Created            int64  `json:"created"`                  // unix timestamp
	LastUsed           int64  `json:"last_used"`                // unix timestamp
	Active             bool   `json:"active"`                   // currently selected
	Backend            string `json:"backend,omitempty"`        // "claude" (default) or "codex"
	Model              string `json:"model,omitempty"`          // last used model (from result)
	ModelOverride      string `json:"model_override,omitempty"`  // user-selected model
	EffortOverride     string `json:"effort_override,omitempty"` // reasoning effort level
	ContextWindow      int    `json:"ctx_window,omitempty"`
	ContextUsed        int    `json:"ctx_used,omitempty"`
	Messages           int    `json:"messages"` // user message count
	PermissionMode     string `json:"permission_mode,omitempty"`
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"`
	RateLimitStatus    string `json:"rl_status,omitempty"`    // "allowed" | "throttled" | "rejected"
	RateLimitResets    int64  `json:"rl_resets,omitempty"`    // unix timestamp
	RateLimitType      string `json:"rl_type,omitempty"`     // "five_hour" | "weekly"
	RateLimitOverage   bool   `json:"rl_overage,omitempty"`   // using overage
}

type ChatSessions struct {
	Sessions []*Session `json:"sessions"`
}

type Store struct {
	mu    sync.Mutex
	Chats map[string]*ChatSessions `json:"chats"`
	path  string
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
	s := &Store{path: path, Chats: make(map[string]*ChatSessions)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}

	// Try new format first.
	if err := json.Unmarshal(data, s); err == nil && len(s.Chats) > 0 {
		return s, nil
	}

	// Fall back to legacy flat format.
	s.Chats = make(map[string]*ChatSessions)
	var legacy struct {
		Sessions []*Session `json:"sessions"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, err
	}
	if len(legacy.Sessions) > 0 {
		s.Chats["_migrated"] = &ChatSessions{Sessions: legacy.Sessions}
	}
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
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

func (s *Store) chat(chatID string) *ChatSessions {
	cs, ok := s.Chats[chatID]
	if !ok {
		cs = &ChatSessions{}
		s.Chats[chatID] = cs
	}
	return cs
}

func (s *Store) SessionsFor(chatID string) []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chat(chatID).Sessions
}

func (s *Store) Active(chatID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.chat(chatID).Sessions {
		if sess.Active {
			return sess
		}
	}
	return nil
}

func (s *Store) New(chatID, name, cwd string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.chat(chatID)
	for _, sess := range cs.Sessions {
		sess.Active = false
	}
	sess := &Session{
		Name:    name,
		CWD:     cwd,
		Created: time.Now().Unix(),
		Active:  true,
	}
	cs.Sessions = append(cs.Sessions, sess)
	return sess
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
	return cs.Sessions[idx]
}
