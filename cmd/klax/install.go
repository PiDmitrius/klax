package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runInstall() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find executable: %v\n", err)
		os.Exit(1)
	}

	home, _ := os.UserHomeDir()
	binDir := filepath.Join(home, "go", "bin")
	os.MkdirAll(binDir, 0755)
	dst := filepath.Join(binDir, "klax")
	if err := copyFile(exe, dst, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot install: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("installed: %s\n", dst)

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	os.MkdirAll(unitDir, 0755)
	unitPath := filepath.Join(unitDir, "klax.service")
	unit := fmt.Sprintf(`[Unit]
Description=klax — Telegram bridge for Claude Code
After=network.target

[Service]
Type=simple
ExecStart=%s start --foreground
Restart=always
RestartSec=5
StartLimitBurst=3
StartLimitIntervalSec=60

[Install]
WantedBy=default.target
`, dst)
	os.WriteFile(unitPath, []byte(unit), 0644)
	fmt.Printf("installed: %s\n", unitPath)

	exec.Command("systemctl", "--user", "daemon-reload").Run()
	exec.Command("systemctl", "--user", "enable", "klax").Run()

	// Write restart marker if not already present (update writes it before build).
	if readMarker() == nil {
		if err := writeMarker("update"); err != nil {
			log.Printf("warning: could not write restart marker: %v", err)
		}
	}

	// Check if the service is currently running.
	out, _ := exec.Command("systemctl", "--user", "is-active", "klax").Output()
	if strings.TrimSpace(string(out)) == "active" {
		fmt.Println("\nInstalled. Service is running — it will restart automatically.")
	} else {
		fmt.Println("\nInstalled. Run: klax start")
	}
}

func runUninstall() {
	exec.Command("systemctl", "--user", "stop", "klax").Run()
	exec.Command("systemctl", "--user", "disable", "klax").Run()
	home, _ := os.UserHomeDir()
	os.Remove(filepath.Join(home, ".config", "systemd", "user", "klax.service"))
	exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Println("uninstalled")
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// Remove old file first to avoid "text file busy" when overwriting a running binary.
	os.Remove(dst)
	return os.WriteFile(dst, data, mode)
}
