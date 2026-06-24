//go:build linux

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
	dst := installBinary()

	home, _ := os.UserHomeDir()
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	os.MkdirAll(unitDir, 0755)
	unitPath := filepath.Join(unitDir, "klax.service")
	unit := renderServiceUnit(dst)
	if drifted, err := unitDrifted(unitPath, unit); err != nil {
		fmt.Fprintf(os.Stderr, "cannot inspect current service unit: %v\n", err)
		os.Exit(1)
	} else if drifted {
		fmt.Fprintf(os.Stderr, "warning: local systemd unit drift detected, overwriting %s\n", tildePath(unitPath))
	}
	if err := verifyServiceUnit(unit); err != nil {
		if ignorableVerifyError(err) {
			fmt.Fprintf(os.Stderr, "warning: service unit verification skipped: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "service unit verification failed: %v\n", err)
			os.Exit(1)
		}
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "cannot install service unit: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("installed: %s\n", tildePath(unitPath))

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

func renderServiceUnit(binPath string) string {
	return fmt.Sprintf(`[Unit]
Description=klax — AI messaging bridge
After=network.target
StartLimitBurst=3
StartLimitIntervalSec=60

[Service]
Type=simple
ExecStart=%s start --foreground
Restart=always
RestartSec=5
# An OOM kill inside the cgroup (a runaway turn subprocess) must not take
# down the daemon and every other session with it; systemd's default
# OOMPolicy=stop would stop the whole unit.
OOMPolicy=continue

[Install]
WantedBy=default.target
`, binPath)
}

func unitDrifted_unused() {}

func verifyServiceUnit(unit string) error {
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		return fmt.Errorf("systemd-analyze not found")
	}

	tmp, err := os.CreateTemp("", "klax-service-*.service")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.WriteString(unit); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	cmd := exec.Command("systemd-analyze", "verify", tmpPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func ignorableVerifyError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "Operation not permitted") ||
		strings.Contains(msg, "SO_PASSCRED failed")
}
