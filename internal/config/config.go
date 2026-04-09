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

// Config is stored at ~/.config/klax/config.json
type Config struct {
	TelegramToken  string  `json:"tg_token"`
	AllowedUsers   []int64 `json:"tg_allowed_users"` // Telegram user IDs
	DefaultCWD     string  `json:"default_cwd"`
	PermissionMode string  `json:"permission_mode"` // acceptEdits | bypassPermissions | auto
	SourceDir      string  `json:"source_dir"`      // path to klax source for local builds

	MaxToken        string  `json:"mx_token,omitempty"`
	MaxAllowedUsers []int64 `json:"mx_allowed_users,omitempty"` // MAX user IDs

	VKToken        string `json:"vk_token,omitempty"`
	VKAllowedUsers []int  `json:"vk_allowed_users,omitempty"` // VK user IDs

	Users              []UserIdentity `json:"users,omitempty"`               // cross-platform identity mapping
	DisabledTransports []string       `json:"disabled_transports,omitempty"` // transports disabled via /transports off
	GroupChats         []GroupChat    `json:"group_chats,omitempty"`         // chats with group mode enabled
}

// GroupChat stores group mode settings for a chat.
type GroupChat struct {
	ID  string `json:"id"`  // chat ID (e.g. "tg:-100123456")
	CWD string `json:"cwd"` // working directory for the group
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
	return &c, nil
}

func Save(c *Config) error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), data, 0600)
}
