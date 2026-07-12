// Package session manages AI coding sessions.
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type ScopeDefaults struct {
	Backend   string `json:"backend,omitempty"`
	Model     string `json:"model,omitempty"`
	Think     string `json:"think,omitempty"`
	Sandbox   string `json:"sandbox,omitempty"`    // "on" | "off"
	ClaudeTTY bool   `json:"claude_tty,omitempty"` // drive Claude through klax tty
}

type Session struct {
	ID            string `json:"id"`                       // session UUID (claude or codex thread_id)
	Name          string `json:"name"`                     // user-friendly name
	CWD           string `json:"cwd"`                      // working directory
	Created       int64  `json:"created"`                  // monotonic per-chat session key (never reused; not a timestamp for new sessions)
	LastUsed      int64  `json:"last_used"`                // unix timestamp
	Active        bool   `json:"active"`                   // currently selected
	Backend       string `json:"backend,omitempty"`        // "claude" (default) or "codex"
	Model         string `json:"model,omitempty"`          // last used model (from result)
	ModelOverride string `json:"model_override,omitempty"` // user-selected model
	ThinkOverride string `json:"think_override,omitempty"` // thinking level
	Sandbox       string `json:"sandbox,omitempty"`        // "on" | "off"
	ClaudeTTY     bool   `json:"claude_tty,omitempty"`     // drive Claude through klax tty
	ContextWindow int    `json:"ctx_window,omitempty"`
	ContextUsed   int    `json:"ctx_used,omitempty"`
	Messages      int    `json:"messages"` // user message count
	// UI read-through watermark — the durable per-session unread cursor: the highest
	// (turn_seq, block index) the user has read. Absent on legacy stores ⇒ 0 ("nothing read
	// yet"). Consumed by the UI so the unread divider/badge/title survive a page reload
	// and a daemon restart instead of re-baselining to "all read".
	ReadThroughTurn    int64  `json:"read_through_turn,omitempty"`
	ReadThroughBlock   int    `json:"read_through_block,omitempty"`
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"`
	// Deprecated: rate limits moved to global config per backend.
	// Keep fields for JSON backward compat (old sessions.json).
	RateLimitStatus  string `json:"rl_status,omitempty"`
	RateLimitResets  int64  `json:"rl_resets,omitempty"`
	RateLimitType    string `json:"rl_type,omitempty"`
	RateLimitOverage bool   `json:"rl_overage,omitempty"`
}

type ChatSessions struct {
	// HighWater is a MONOTONIC per-chat counter: the highest session key ever handed out in this chat.
	// It only increases and is NEVER reused, so a deleted session's key can't be reassigned — reuse
	// would bind a new session to the deleted one's canonical (removed) durable Store (writes returning
	// ErrRemoved) or merge runs. It survives deletion and is persisted in sessions.json; on load it is
	// lifted to the max existing key (normalize) so migrated stores keep their keys and new ones simply
	// continue above them.
	HighWater int64      `json:"high_water,omitempty"`
	Sessions  []*Session `json:"sessions"`
}

// nextCreated advances the high-water and returns the new session key (Session.Created — the canonical
// key for the runner map and the durable-store registry; despite the name it is a monotonic counter,
// not a timestamp, for new sessions).
func (cs *ChatSessions) nextCreated() int64 {
	cs.HighWater++
	return cs.HighWater
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
		payload.Chats[key] = &ChatSessions{Sessions: cloneSessions(chat.Sessions), HighWater: chat.HighWater}
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
			// Legacy stores had no HighWater: lift it to at least the max existing key, so migrated stores
			// keep their keys and new ones continue above them (deleted-session keys from before this field
			// existed are unrecoverable, but a fresh process starts with an empty Store registry, so a reused
			// key there binds a fresh Store — harmless — while going forward HighWater prevents reuse).
			if sess.Created > chat.HighWater {
				chat.HighWater = sess.Created
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

// EachSession calls fn for every (chatID, Created) in the store under the lock — used at startup to
// rebuild derived indexes (e.g. the file-token index) from each session's on-disk state.
func (s *Store) EachSession(fn func(chatID string, created int64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for chatID, cs := range s.Chats {
		for _, sess := range cs.Sessions {
			if sess != nil {
				fn(chatID, sess.Created)
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
	if def.Sandbox == "" {
		def.Sandbox = fallback.Sandbox
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

// Get returns a clone of the session identified by Created within chatID.
// Returns nil if no matching session exists.
func (s *Store) Get(chatID string, created int64) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.chat(chatID).Sessions {
		if sess.Created == created {
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
	if def.Sandbox == "" {
		def.Sandbox = defaults.Sandbox
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
	created := cs.nextCreated()
	sess := &Session{
		Name:          name,
		CWD:           cwd,
		Created:       created,
		Active:        true,
		Backend:       def.Backend,
		ModelOverride: def.Model,
		ThinkOverride: def.Think,
		Sandbox:       def.Sandbox,
		ClaudeTTY:     def.ClaudeTTY,
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
	if def.Sandbox == "" {
		def.Sandbox = defaults.Sandbox
	}
	for _, sess := range cs.Sessions {
		sess.Active = false
	}
	created := cs.nextCreated()
	sess := &Session{
		Name:          name,
		CWD:           cwd,
		Created:       created,
		Active:        true,
		Backend:       def.Backend,
		ModelOverride: def.Model,
		ThinkOverride: def.Think,
		Sandbox:       def.Sandbox,
		ClaudeTTY:     def.ClaudeTTY,
	}
	cs.Sessions = append(cs.Sessions, sess)
	return cloneSession(sess)
}

// Add inserts an ALREADY-FORMED session into a chat ATOMICALLY: under a single lock it deactivates
// the current active session, assigns a unique Created, marks the new one active, and appends it. No
// intermediate or partially-configured state is ever visible to a concurrent SessionsFor — the whole
// session is published in one operation. The store takes ownership of `sess`; a clone is returned.
func (s *Store) Add(chatID string, sess *Session) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.chat(chatID)
	for _, existing := range cs.Sessions {
		existing.Active = false
	}
	sess.Created = cs.nextCreated()
	sess.Active = true
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

// Reorder rearranges a chat's sessions to match the given order of Created ids
// (the tab strip's drag-and-drop). Ids not present are ignored; sessions omitted
// from the list keep their relative order after the listed ones, so a partial or
// stale order can never drop a tab. Returns true if the order actually changed.
func (s *Store) Reorder(chatID string, order []int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.chat(chatID)
	if len(cs.Sessions) < 2 {
		return false
	}
	rank := make(map[int64]int, len(order))
	for i, id := range order {
		if _, seen := rank[id]; !seen {
			rank[id] = i
		}
	}
	orig := make(map[int64]int, len(cs.Sessions))
	for i, sess := range cs.Sessions {
		orig[sess.Created] = i
	}
	sorted := make([]*Session, len(cs.Sessions))
	copy(sorted, cs.Sessions)
	sort.SliceStable(sorted, func(a, b int) bool {
		ra, oka := rank[sorted[a].Created]
		rb, okb := rank[sorted[b].Created]
		if oka && okb {
			return ra < rb
		}
		if oka != okb {
			return oka // ids present in the requested order come first
		}
		return orig[sorted[a].Created] < orig[sorted[b].Created]
	})
	changed := false
	for i := range sorted {
		if sorted[i] != cs.Sessions[i] {
			changed = true
			break
		}
	}
	if !changed {
		return false
	}
	cs.Sessions = sorted
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
