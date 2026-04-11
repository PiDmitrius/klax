package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/PiDmitrius/klax/internal/config"
	"github.com/PiDmitrius/klax/internal/max"
	"github.com/PiDmitrius/klax/internal/tg"
	"github.com/PiDmitrius/klax/internal/vk"
)

func runSetup() {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("klax setup")
	fmt.Println("----------")

	home, _ := os.UserHomeDir()
	cfg := &config.Config{
		TelegramToken:      "",
		AllowedUsers:       []int64{},
		DefaultCWD:         home,
		SourceDir:          "",
		DefaultBackend:     "claude",
		Backends:           map[string]config.BackendConfig{"claude": {}, "codex": {}},
		MaxToken:           "",
		MaxAllowedUsers:    []int64{},
		VKToken:            "",
		VKAllowedUsers:     []int{},
		Users:              []config.UserIdentity{},
		DisabledTransports: []string{},
		GroupChats:         []config.GroupChat{},
	}

	cfg.TelegramToken = promptValidatedToken(reader, "Telegram bot token [skip]: ", func(token string) error {
		return tg.New(token).GetMe()
	})
	if cfg.TelegramToken != "" {
		cfg.AllowedUsers = promptInt64List(reader, "Telegram allowed users (comma-separated): ")
	}

	cfg.MaxToken = promptValidatedToken(reader, "MAX bot token [skip]: ", func(token string) error {
		_, err := max.New(token).GetMe()
		return err
	})
	if cfg.MaxToken != "" {
		cfg.MaxAllowedUsers = promptInt64List(reader, "MAX allowed users (comma-separated): ")
	}

	cfg.VKToken = promptValidatedToken(reader, "VK group token [skip]: ", func(token string) error {
		_, err := vk.New(token).GetMe()
		return err
	})
	if cfg.VKToken != "" {
		cfg.VKAllowedUsers = promptIntList(reader, "VK allowed users (comma-separated): ")
	}

	fmt.Print("Default working directory [~]: ")
	cwd, _ := reader.ReadString('\n')
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cwd == "~" {
		cwd = home
	}
	cfg.DefaultCWD = cwd

	if err := config.Save(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Saved to %s\n", filepath.Join(config.Dir(), "config.json"))
	fmt.Println("Next: klax install && klax start")
}

func promptValidatedToken(reader *bufio.Reader, prompt string, validate func(string) error) string {
	for {
		fmt.Print(prompt)
		token, _ := reader.ReadString('\n')
		token = strings.TrimSpace(token)
		if token == "" {
			return ""
		}
		if err := validate(token); err != nil {
			fmt.Printf("Validation failed: %v\n", err)
			continue
		}
		fmt.Println("OK")
		return token
	}
}

func promptInt64List(reader *bufio.Reader, prompt string) []int64 {
	for {
		fmt.Print(prompt)
		line, _ := reader.ReadString('\n')
		values, err := parseInt64List(line)
		if err != nil {
			fmt.Printf("Invalid list: %v\n", err)
			continue
		}
		return values
	}
}

func promptIntList(reader *bufio.Reader, prompt string) []int {
	for {
		fmt.Print(prompt)
		line, _ := reader.ReadString('\n')
		values, err := parseIntList(line)
		if err != nil {
			fmt.Printf("Invalid list: %v\n", err)
			continue
		}
		return values
	}
}

func parseInt64List(line string) ([]int64, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return []int64{}, nil
	}
	parts := strings.Split(line, ",")
	values := make([]int64, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, nil
}

func parseIntList(line string) ([]int, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return []int{}, nil
	}
	parts := strings.Split(line, ",")
	values := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, nil
}
