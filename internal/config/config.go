package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// UserIdentity maps platform-specific IDs to a canonical user.
// Sessions in DMs are shared across platforms for the same user.
type UserIdentity struct {
	ID         string `json:"id"`              // canonical user ID (e.g. "claw")
	TelegramID int64  `json:"tg_id,omitempty"` // Telegram user ID
	MaxID      int64  `json:"mx_id,omitempty"` // MAX user ID
	VKID       int64  `json:"vk_id,omitempty"` // VK user ID
}

// BackendConfig holds per-backend settings.
type BackendConfig struct {
	PermissionMode string `json:"permission_mode"` // claude: acceptEdits | bypassPermissions | auto
	Sandbox        string `json:"sandbox"`         // codex: read-only | workspace-write | danger-full-access
	FullAuto       bool   `json:"full_auto"`       // codex: --full-auto shortcut
}

// Config is stored at ~/.config/klax/config.json
type Config struct {
	TelegramToken string  `json:"tg_token"`
	AllowedUsers  []int64 `json:"tg_allowed_users"` // Telegram user IDs
	DefaultCWD    string  `json:"default_cwd"`
	SourceDir     string  `json:"source_dir"` // path to klax source for local builds

	// Legacy field — migrated to Backends["claude"].PermissionMode on load.
	PermissionMode string `json:"permission_mode,omitempty"`

	// Backend settings.
	DefaultBackend string                   `json:"default_backend"` // "claude" (default) or "codex"
	Backends       map[string]BackendConfig `json:"backends"`

	MaxToken        string  `json:"mx_token"`
	MaxAllowedUsers []int64 `json:"mx_allowed_users"` // MAX user IDs

	VKToken        string `json:"vk_token"`
	VKAllowedUsers []int  `json:"vk_allowed_users"` // VK user IDs

	Users              []UserIdentity `json:"users"`               // cross-platform identity mapping
	DisabledTransports []string       `json:"disabled_transports"` // transports disabled via /transports off
	GroupChats         []GroupChat    `json:"group_chats"`         // chats with group mode enabled
}

// GroupChat stores group mode settings for a chat.
type GroupChat struct {
	ID  string `json:"id"`  // chat ID (e.g. "tg:-100123456")
	CWD string `json:"cwd"` // working directory for the group
}

// GetDefaultBackend returns the default backend name.
func (c *Config) GetDefaultBackend() string {
	if c.DefaultBackend != "" {
		return c.DefaultBackend
	}
	return "claude"
}

// BackendFor returns config for a named backend, falling back to legacy fields.
func (c *Config) BackendFor(name string) BackendConfig {
	if c.Backends != nil {
		if bc, ok := c.Backends[name]; ok {
			if name == "codex" && !bc.FullAuto && bc.Sandbox == "" {
				bc.Sandbox = "danger-full-access"
			}
			return bc
		}
	}
	// Fallback: build from legacy fields.
	if name == "claude" {
		return BackendConfig{PermissionMode: c.PermissionMode}
	}
	if name == "codex" {
		return BackendConfig{Sandbox: "danger-full-access"}
	}
	return BackendConfig{}
}

func Dir() string {
	if d := os.Getenv("KLAX_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "klax")
}

func Load() (*Config, error) {
	path := filepath.Join(Dir(), "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	// Migrate legacy permission_mode into backends.
	if c.Backends == nil && c.PermissionMode != "" {
		c.Backends = map[string]BackendConfig{
			"claude": {PermissionMode: c.PermissionMode},
		}
	}
	return normalize(&c), nil
}

func Save(c *Config) error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(normalize(c), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), data, 0600)
}

func normalize(c *Config) *Config {
	if c == nil {
		c = &Config{}
	}
	if c.DefaultBackend == "" {
		c.DefaultBackend = "claude"
	}
	if c.Backends == nil {
		c.Backends = make(map[string]BackendConfig)
	}
	if _, ok := c.Backends["claude"]; !ok {
		c.Backends["claude"] = BackendConfig{}
	}
	if _, ok := c.Backends["codex"]; !ok {
		c.Backends["codex"] = BackendConfig{}
	}
	if c.AllowedUsers == nil {
		c.AllowedUsers = []int64{}
	}
	if c.MaxAllowedUsers == nil {
		c.MaxAllowedUsers = []int64{}
	}
	if c.VKAllowedUsers == nil {
		c.VKAllowedUsers = []int{}
	}
	if c.Users == nil {
		c.Users = []UserIdentity{}
	}
	if c.DisabledTransports == nil {
		c.DisabledTransports = []string{}
	}
	if c.GroupChats == nil {
		c.GroupChats = []GroupChat{}
	}
	return c
}
