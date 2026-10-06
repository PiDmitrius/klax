// Package session manages coding sessions. Persisted mutations hold the store lock through
// saving and restore the prior value on failure; unchanged mutations do not write.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/PiDmitrius/klax/internal/ids"
)

type ScopeDefaults struct {
	Backend        string `json:"backend,omitempty"`
	ModelRequested string `json:"model_requested,omitempty"`
	Think          string `json:"think,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"` // "on" | "off"
	TTY            bool   `json:"tty,omitempty"`     // drive Claude through klax tty
	CWD            string `json:"cwd,omitempty"`     // working directory for the next new session
	// YM threads inherit group mode once, then evolve independently. Keep that
	// per-thread state with the thread's sessions instead of materializing every
	// discovered thread in config.json's explicit group_chats registry.
	GroupMode           *bool  `json:"group_mode,omitempty"`
	GroupVerbose        *bool  `json:"group_verbose,omitempty"`
	GroupAttachmentMode string `json:"group_attachment_mode,omitempty"` // "off" | "on" (default) | "any"
}

type Session struct {
	KlaxID         string `json:"klax_id"`              // opaque session id (internal/ids), unique across the store
	BackendID      string `json:"backend_id,omitempty"` // backend session identity (claude session / codex thread)
	Name           string `json:"name"`
	CWD            string `json:"cwd"`
	LastUsed       int64  `json:"last_used"` // unix timestamp
	Active         bool   `json:"active"`    // the chat's selected session
	Backend        string `json:"backend,omitempty"`
	ModelUsed      string `json:"model_used,omitempty"`      // model the backend last reported
	ModelRequested string `json:"model_requested,omitempty"` // model the user selected
	Think          string `json:"think,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"` // "on" | "off"
	TTY            bool   `json:"tty,omitempty"`     // drive Claude through klax tty
	ContextWindow  int    `json:"ctx_window,omitempty"`
	ContextUsed    int    `json:"ctx_used,omitempty"`
	Messages       int    `json:"messages"` // user message count
	// Groups label a session for the UI's filtered views (one browser tab per group). A pure view
	// filter: never a second source of order or session state, and never a place for computed
	// "is:*" views, which are derived from live facts instead of stored here. A session may belong
	// to several groups.
	Groups []string `json:"groups,omitempty"`
	// Durable read positions "<turn_seq>.<block_seq>" per access role: the highest block the user
	// has read, so unread state survives reloads and restarts. Empty means nothing read yet.
	ReadPos      string `json:"read_pos,omitempty"`
	ReadPosRO    string `json:"read_pos_ro,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"` // additional backend instructions
}

// FormatReadPos renders a read position.
func FormatReadPos(turn int64, block int) string {
	if turn == 0 && block == 0 {
		return ""
	}
	return strconv.FormatInt(turn, 10) + "." + strconv.Itoa(block)
}

// ParseReadPos reads "<turn_seq>.<block_seq>"; the empty string is the start (0, 0).
func ParseReadPos(v string) (turn int64, block int, err error) {
	if v == "" {
		return 0, 0, nil
	}
	t, b, ok := strings.Cut(v, ".")
	if !ok {
		return 0, 0, fmt.Errorf("read_pos %q: want <turn_seq>.<block_seq>", v)
	}
	if turn, err = strconv.ParseInt(t, 10, 64); err == nil {
		block, err = strconv.Atoi(b)
	}
	if err != nil || block < 0 {
		return 0, 0, fmt.Errorf("read_pos %q: want <turn_seq>.<block_seq>", v)
	}
	return turn, block, nil
}

// ReadPosition selects the durable read position for the access role, independent of token rotation.
func (s *Session) ReadPosition(readOnly bool) (int64, int) {
	v := s.ReadPos
	if readOnly {
		v = s.ReadPosRO
	}
	turn, block, _ := ParseReadPos(v)
	return turn, block
}

// AdvanceReadPos raises the role's read position; it never moves back.
func (s *Session) AdvanceReadPos(readOnly bool, turn int64, block int) bool {
	t, b := s.ReadPosition(readOnly)
	if turn < t || (turn == t && block <= b) {
		return false
	}
	if readOnly {
		s.ReadPosRO = FormatReadPos(turn, block)
	} else {
		s.ReadPos = FormatReadPos(turn, block)
	}
	return true
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

// newKlaxID returns an id no session in the store holds. Caller holds s.mu.
func (s *Store) newKlaxID() string {
	for {
		id := ids.New()
		if !s.hasKlaxIDLocked(id) {
			return id
		}
	}
}

func (s *Store) hasKlaxIDLocked(id string) bool {
	for _, cs := range s.Chats {
		for _, sess := range cs.Sessions {
			if sess != nil && sess.KlaxID == id {
				return true
			}
		}
	}
	return false
}

func cloneSession(sess *Session) *Session {
	if sess == nil {
		return nil
	}
	cp := *sess
	if len(sess.Groups) > 0 { // a shared backing array would let a caller mutate the stored session
		cp.Groups = append([]string(nil), sess.Groups...)
	}
	return &cp
}

func cloneDefaults(def *ScopeDefaults) *ScopeDefaults {
	if def == nil {
		return nil
	}
	cp := *def
	if def.GroupMode != nil {
		enabled := *def.GroupMode
		cp.GroupMode = &enabled
	}
	if def.GroupVerbose != nil {
		verbose := *def.GroupVerbose
		cp.GroupVerbose = &verbose
	}
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

// ErrLegacyStore means sessions.json is still in the pre-klax_id format: the store migration
// (sessfiles.MigrateStore) must run before the store is loaded.
var ErrLegacyStore = errors.New("sessions.json is in the legacy format; run the store migration first")

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
	if IsLegacy(data) {
		return nil, ErrLegacyStore
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	s.normalize()
	return s, nil
}

// IsLegacy reports whether a sessions.json payload predates klax_id: a flat or high-water store,
// a session without klax_id, or a template with the old field names.
func IsLegacy(data []byte) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return false
	}
	if _, ok := top["high_water"]; ok {
		return true
	}
	if _, ok := top["sessions"]; ok {
		return true
	}
	var chats map[string]struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	json.Unmarshal(top["chats"], &chats)
	for _, cs := range chats {
		for _, sess := range cs.Sessions {
			if _, ok := sess["klax_id"]; !ok {
				return true
			}
		}
	}
	var scope map[string]map[string]json.RawMessage
	json.Unmarshal(top["scope_defaults"], &scope)
	for _, def := range scope {
		for _, k := range []string{"model", "claude_tty", "group_attachments"} {
			if _, ok := def[k]; ok {
				return true
			}
		}
	}
	return false
}

// MigrateTo moves legacy sessions to the given chatID.
func (s *Store) MigrateTo(chatID string) (bool, error) {
	return s.MergeKeys(chatID, []string{"_migrated"})
}

// MergeKeys merges sessions from oldKeys into targetKey.
// Directories are moved durably before metadata is saved. A failed startup migration
// must be retried before any session stores are opened.
func (s *Store) MergeKeys(targetKey string, oldKeys []string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := false
	for _, old := range oldKeys {
		if cs, ok := s.Chats[old]; ok && old != targetKey {
			merged = true
			if err := moveSessionDirs(old, targetKey, cs.Sessions); err != nil {
				return false, err
			}
		}
	}
	if !merged {
		return false, nil
	}
	prior := s.Chats
	s.Chats = make(map[string]*ChatSessions, len(prior))
	for key, cs := range prior {
		s.Chats[key] = &ChatSessions{Sessions: cloneSessions(cs.Sessions)}
	}
	target := s.chat(targetKey)
	for _, old := range oldKeys {
		cs, ok := s.Chats[old]
		if !ok || old == targetKey {
			continue
		}
		target.Sessions = append(target.Sessions, cs.Sessions...)
		delete(s.Chats, old)
	}
	// Ensure at most one session is active.
	foundActive := false
	for i := len(target.Sessions) - 1; i >= 0; i-- {
		if target.Sessions[i].Active {
			if foundActive {
				target.Sessions[i].Active = false
			}
			foundActive = true
		}
	}
	if err := s.saveLocked(); err != nil {
		s.Chats = prior
		return false, err
	}
	return true, nil
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
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

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".sessions-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return dir.Sync()
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

// EachSession calls fn for every (chatID, klaxID) in the store under the lock — used at startup to
// rebuild derived indexes (e.g. the file-token index) from each session's on-disk state.
func (s *Store) EachSession(fn func(chatID, klaxID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for chatID, cs := range s.Chats {
		for _, sess := range cs.Sessions {
			if sess != nil {
				fn(chatID, sess.KlaxID)
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

// ErrSessionNotFound is returned by UpdateSessionChecked when klaxID has no match —
// e.g. the session was deleted (/nuke, /new) between an earlier lookup and this call.
var ErrSessionNotFound = errors.New("session not found")

// UpdateSessionChecked applies fn to the session identified by klaxID only if check
// passes, both evaluated under the SAME lock — closing the gap between a precondition
// verified earlier (e.g. Messages==0) and the mutation, during which a message could
// have started and finished running. check may inspect but must not mutate sess; it
// runs even when fn would be a no-op, so a failing check always short-circuits fn.
// Returns the resulting session (unmodified if check failed) and check's error, or
// ErrSessionNotFound if klaxID has no match.
func (s *Store) UpdateSessionChecked(chatID, klaxID string, check func(*Session) error, fn func(*Session)) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateSessionCheckedLocked(chatID, klaxID, check, fn)
}

func (s *Store) updateSessionCheckedLocked(chatID, klaxID string, check func(*Session) error, fn func(*Session)) (*Session, error) {
	for _, sess := range s.chat(chatID).Sessions {
		if sess.KlaxID == klaxID {
			if check != nil {
				if err := check(sess); err != nil {
					return cloneSession(sess), err
				}
			}
			fn(sess)
			return cloneSession(sess), nil
		}
	}
	return nil, ErrSessionNotFound
}

func (s *Store) UpdateSessionPersisted(chatID, klaxID string, check func(*Session) error, fn func(*Session)) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var updated *Session
	err := s.persistChatLocked(chatID, func() error {
		var err error
		updated, err = s.updateSessionCheckedLocked(chatID, klaxID, check, fn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) persistChatLocked(chatID string, edit func() error) error {
	old, existed := s.Chats[chatID]
	var prior *ChatSessions
	if existed {
		prior = &ChatSessions{Sessions: cloneSessions(old.Sessions)}
	}
	err := edit()
	if err == nil && !reflect.DeepEqual(prior, s.Chats[chatID]) {
		err = s.saveLocked()
	}
	if err != nil {
		if existed {
			s.Chats[chatID] = prior
		} else {
			delete(s.Chats, chatID)
		}
	}
	return err
}

// SetCWDIfMessages0 re-checks Messages==0 for the session identified by klaxID and,
// if still true, sets both its CWD and the chat's ScopeDefaults.CWD under one lock —
// closing the same TOCTOU gap as UpdateSessionChecked, specifically for /cwd (which
// writes both fields together, unlike a plain UI settings patch). Returns the updated
// session and true, or the current session and false if Messages>0 by now.
func (s *Store) SetCWDIfMessages0(chatID, klaxID string, cwd string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.chat(chatID).Sessions {
		if sess.KlaxID == klaxID {
			if sess.Messages > 0 {
				return cloneSession(sess), false
			}
			sess.CWD = cwd
			s.scope(chatID).CWD = cwd
			return cloneSession(sess), true
		}
	}
	return nil, false
}

func (s *Store) UpdateSession(chatID, klaxID string, fn func(*Session)) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.chat(chatID).Sessions {
		if sess.KlaxID == klaxID {
			fn(sess)
			return cloneSession(sess)
		}
	}
	return nil
}

// Get returns a clone of the session identified by klaxID within chatID.
// Returns nil if no matching session exists.
func (s *Store) Get(chatID, klaxID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.chat(chatID).Sessions {
		if sess.KlaxID == klaxID {
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
			return cloneSession(sess)
		}
	}
	for _, sess := range cs.Sessions {
		sess.Active = false
	}
	sess := &Session{
		KlaxID:         s.newKlaxID(),
		Name:           name,
		CWD:            cwd,
		Active:         true,
		Backend:        def.Backend,
		ModelRequested: def.ModelRequested,
		Think:          def.Think,
		Sandbox:        def.Sandbox,
		TTY:            def.TTY,
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
	sess := &Session{
		KlaxID:         s.newKlaxID(),
		Name:           name,
		CWD:            cwd,
		Active:         true,
		Backend:        def.Backend,
		ModelRequested: def.ModelRequested,
		Think:          def.Think,
		Sandbox:        def.Sandbox,
		TTY:            def.TTY,
	}
	cs.Sessions = append(cs.Sessions, sess)
	return cloneSession(sess)
}

// Add inserts an ALREADY-FORMED session into a chat ATOMICALLY: under a single lock it deactivates
// the current active session, assigns a unique KlaxID, marks the new one active, and appends it. No
// intermediate or partially-configured state is ever visible to a concurrent SessionsFor — the whole
// session is published in one operation. The store takes ownership of `sess`; a clone is returned.
func (s *Store) Add(chatID string, sess *Session) *Session {
	return s.AddWithDefaults(chatID, sess, nil)
}

// AddWithDefaults atomically publishes an already-formed session AND, when defaults is non-nil,
// records that same session's new-session template. Keeping both mutations under one lock means two
// concurrent creates cannot leave the older session's defaults as the final template.
func (s *Store) AddWithDefaults(chatID string, sess *Session, defaults *ScopeDefaults) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addWithDefaultsLocked(chatID, sess, defaults)
}

func (s *Store) addWithDefaultsLocked(chatID string, sess *Session, defaults *ScopeDefaults) *Session {
	cs := s.chat(chatID)
	for _, existing := range cs.Sessions {
		existing.Active = false
	}
	sess.KlaxID = s.newKlaxID()
	sess.Active = true
	cs.Sessions = append(cs.Sessions, sess)
	if defaults != nil {
		*s.scope(chatID) = *defaults
	}
	return cloneSession(sess)
}

func (s *Store) DeleteByID(chatID, klaxID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for idx, sess := range s.chat(chatID).Sessions {
		if sess.KlaxID == klaxID {
			return s.deleteLocked(chatID, idx)
		}
	}
	return false
}

func (s *Store) Delete(chatID string, idx int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(chatID, idx)
}

func (s *Store) deleteLocked(chatID string, idx int) bool {
	cs := s.chat(chatID)
	if idx < 0 || idx >= len(cs.Sessions) {
		return false
	}
	cs.Sessions = append(cs.Sessions[:idx], cs.Sessions[idx+1:]...)
	return true
}

// Reorder rearranges a chat's sessions to match the given order of klax ids (the tab strip's
// drag-and-drop). The order may be a SUBSET — a filtered group view drags only the tabs it shows — so
// the permutation is SLOT-PRESERVING: the positions the listed sessions occupied are refilled in the
// requested order, and every session not listed keeps its exact index.
//
// That is what makes one global order enough for every view: the relative order of any pair changes
// only if BOTH of them were listed, so dragging inside one group cannot disturb another group whose
// members it does not share. With a FULL list the occupied slots are all positions, so the result is
// simply the requested order — the root strip's behaviour is unchanged.
//
// Unknown ids are ignored, so a stale client order can never drop or resurrect a tab. Returns true
// if the order actually changed.
func (s *Store) Reorder(chatID string, order []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reorderLocked(chatID, order)
}

func (s *Store) ReorderPersisted(chatID string, order []string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var changed bool
	err := s.persistChatLocked(chatID, func() error {
		changed = s.reorderLocked(chatID, order)
		return nil
	})
	return changed && err == nil, err
}

var ErrLastSession = errors.New("cannot close the last session")

func (s *Store) ClosePersisted(chatID, klaxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistChatLocked(chatID, func() error {
		cs := s.chat(chatID)
		for idx, sess := range cs.Sessions {
			if sess.KlaxID != klaxID {
				continue
			}
			if len(cs.Sessions) <= 1 {
				return ErrLastSession
			}
			s.deleteLocked(chatID, idx)
			if sess.Active {
				cs.Sessions[0].Active = true
			}
			return nil
		}
		return ErrSessionNotFound
	})
}

func (s *Store) reorderLocked(chatID string, order []string) bool {
	cs := s.chat(chatID)
	if len(cs.Sessions) < 2 {
		return false
	}
	byID := make(map[string]*Session, len(cs.Sessions))
	for _, sess := range cs.Sessions {
		byID[sess.KlaxID] = sess
	}
	seq := make([]*Session, 0, len(order)) // listed sessions that really exist, request order, deduped
	listed := make(map[string]bool, len(order))
	for _, id := range order {
		sess := byID[id]
		if sess == nil || listed[id] {
			continue
		}
		listed[id] = true
		seq = append(seq, sess)
	}
	sorted := make([]*Session, len(cs.Sessions))
	copy(sorted, cs.Sessions)
	k := 0
	for i, sess := range cs.Sessions {
		if listed[sess.KlaxID] {
			sorted[i] = seq[k]
			k++
		}
	}
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

func (s *Store) AddPersisted(chatID string, sess *Session, defaults *ScopeDefaults) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldChat, hadChat := s.Chats[chatID]
	var prior *ChatSessions
	if hadChat {
		cp := *oldChat
		cp.Sessions = cloneSessions(oldChat.Sessions)
		prior = &cp
	}
	oldDefaults, hadDefaults := s.Scope[chatID]
	priorDefaults := cloneDefaults(oldDefaults)
	added := s.addWithDefaultsLocked(chatID, sess, defaults)
	if err := s.saveLocked(); err != nil {
		if hadChat {
			s.Chats[chatID] = prior
		} else {
			delete(s.Chats, chatID)
		}
		if hadDefaults {
			s.Scope[chatID] = priorDefaults
		} else {
			delete(s.Scope, chatID)
		}
		return nil, err
	}
	return added, nil
}
