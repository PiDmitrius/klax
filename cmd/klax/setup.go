package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/PiDmitrius/klax/internal/config"
)

func runSetup() {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("klax setup")
	fmt.Println("----------")

	cfg := &config.Config{}

	fmt.Print("Telegram bot token: ")
	token, _ := reader.ReadString('\n')
	cfg.TelegramToken = strings.TrimSpace(token)

	fmt.Print("Your Telegram user ID (from @userinfobot): ")
	var uid int64
	fmt.Scan(&uid)
	cfg.AllowedUsers = []int64{uid}

	fmt.Print("Default working directory [~]: ")
	reader.ReadString('\n') // consume newline after Scan
	cwd, _ := reader.ReadString('\n')
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cwd == "~" {
		cwd, _ = os.UserHomeDir()
	}
	cfg.DefaultCWD = cwd
	cfg.PermissionMode = "bypassPermissions"

	if err := config.Save(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Saved to %s\n", filepath.Join(config.Dir(), "config.json"))
	fmt.Println("Next: klax install && klax start")
}
