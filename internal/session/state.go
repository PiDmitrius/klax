package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// RateLimitState stores one rate limit (e.g. five_hour or seven_day).
type RateLimitState struct {
	Status         string `json:"status,omitempty"`    // "allowed" | "allowed_warning" | "throttled" | "rejected"
	ResetsAt       int64  `json:"resets_at,omitempty"` // unix timestamp
	IsUsingOverage bool   `json:"overage,omitempty"`
}

// BackendState holds runtime state for a backend.
type BackendState struct {
	RateLimit5h *RateLimitState `json:"rl_5h,omitempty"`
	RateLimitWk *RateLimitState `json:"rl_wk,omitempty"`
}

// State holds runtime state (not user config, not session data).
// Stored in ~/.local/share/klax/state.json.
type State struct {
	mu       sync.Mutex
	path     string
	Backends map[string]*BackendState `json:"backends,omitempty"`
}

func LoadState() *State {
	path := filepath.Join(StoreDir(), "state.json")
	s := &State{path: path, Backends: make(map[string]*BackendState)}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	json.Unmarshal(data, s)
	if s.Backends == nil {
		s.Backends = make(map[string]*BackendState)
	}
	return s
}

func (s *State) Save() error {
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

// Backend returns the state for a backend, creating it if needed.
func (s *State) Backend(name string) *BackendState {
	s.mu.Lock()
	defer s.mu.Unlock()
	bs, ok := s.Backends[name]
	if !ok {
		bs = &BackendState{}
		s.Backends[name] = bs
	}
	return bs
}
