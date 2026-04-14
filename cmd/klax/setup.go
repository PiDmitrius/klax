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

	// Load existing config if present; start fresh otherwise.
	cfg, err := config.Load()
	if err != nil {
		if os.IsNotExist(err) {
			cfg = &config.Config{DefaultCWD: home}
		} else {
			fmt.Fprintf(os.Stderr, "error: cannot load config: %v\n", err)
			os.Exit(1)
		}
	}

	// Show current values in prompts so the user knows what's already set.
	cfg.TelegramToken = promptValidatedTokenKeep(reader, "Telegram bot token", cfg.TelegramToken, func(token string) error {
		return tg.New(token).GetMe()
	})
	cfg.AllowedUsers = promptInt64ListKeep(reader, "Telegram allowed users", cfg.AllowedUsers)

	cfg.MaxToken = promptValidatedTokenKeep(reader, "MAX bot token", cfg.MaxToken, func(token string) error {
		_, err := max.New(token).GetMe()
		return err
	})
	cfg.MaxAllowedUsers = promptInt64ListKeep(reader, "MAX allowed users", cfg.MaxAllowedUsers)

	cfg.VKToken = promptValidatedTokenKeep(reader, "VK group token", cfg.VKToken, func(token string) error {
		_, err := vk.New(token).GetMe()
		return err
	})
	cfg.VKAllowedUsers = promptIntListKeep(reader, "VK allowed users", cfg.VKAllowedUsers)

	cfg.DefaultCWD = expandPathValue(promptStringKeep(reader, "Default working directory", displayPathValue(cfg.DefaultCWD, home)), home)

	if err := config.Save(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Saved to %s\n", filepath.Join(config.Dir(), "config.json"))
	fmt.Println("Next: klax install && klax start")
}

// promptValidatedTokenKeep shows the current value (masked) and keeps it on empty input.
func promptValidatedTokenKeep(reader *bufio.Reader, label, current string, validate func(string) error) string {
	hint := "enter=keep empty"
	if current != "" {
		hint = "enter=keep " + maskToken(current)
	}
	hint += ", -=clear"
	for {
		fmt.Printf("%s [%s]: ", label, hint)
		token, _ := reader.ReadString('\n')
		token = strings.TrimSpace(token)
		if token == "" {
			return current
		}
		if token == "-" {
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

func maskToken(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "***" + s[len(s)-4:]
}

func promptStringKeep(reader *bufio.Reader, label, current string) string {
	hint := "enter=keep empty"
	if current != "" {
		hint = "enter=keep " + current
	}
	fmt.Printf("%s [%s, -=clear]: ", label, hint)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	switch line {
	case "":
		return current
	case "-":
		return ""
	default:
		return line
	}
}

func promptInt64ListKeep(reader *bufio.Reader, label string, current []int64) []int64 {
	hint := "enter=keep empty"
	if len(current) > 0 {
		hint = "enter=keep " + formatInt64List(current)
	}
	for {
		fmt.Printf("%s [%s, -=clear, comma-separated]: ", label, hint)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return current
		}
		if line == "-" {
			return []int64{}
		}
		values, err := parseInt64List(line)
		if err != nil {
			fmt.Printf("Invalid list: %v\n", err)
			continue
		}
		return values
	}
}

func promptIntListKeep(reader *bufio.Reader, label string, current []int) []int {
	hint := "enter=keep empty"
	if len(current) > 0 {
		hint = "enter=keep " + formatIntList(current)
	}
	for {
		fmt.Printf("%s [%s, -=clear, comma-separated]: ", label, hint)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return current
		}
		if line == "-" {
			return []int{}
		}
		values, err := parseIntList(line)
		if err != nil {
			fmt.Printf("Invalid list: %v\n", err)
			continue
		}
		return values
	}
}

func displayPathValue(path, home string) string {
	if path == "" {
		return ""
	}
	if path == home {
		return "~"
	}
	return tildePath(path)
}

func expandPathValue(path, home string) string {
	switch {
	case path == "", path == "-", home == "":
		return path
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:])
	default:
		return path
	}
}

func formatInt64List(values []int64) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.FormatInt(v, 10))
	}
	return strings.Join(parts, ",")
}

func formatIntList(values []int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ",")
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
