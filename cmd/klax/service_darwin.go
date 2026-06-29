//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func runServiceStart() {
	if _, err := os.Stat(launchAgentPath()); err != nil {
		fmt.Fprintf(os.Stderr, "failed: LaunchAgent not installed\nTry 'klax install' first, or 'klax start --foreground'\n")
		os.Exit(1)
	}
	// Bootstrap loads the plist (RunAtLoad starts it); ignore "already loaded".
	exec.Command("launchctl", "bootstrap", launchdDomain(), launchAgentPath()).Run()
	// kickstart guarantees it is running even if it was loaded but stopped.
	cmd := exec.Command("launchctl", "kickstart", launchdTarget())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "failed: %v\nTry 'klax install' first, or 'klax start --foreground'\n", err)
		os.Exit(1)
	}
	fmt.Println("klax started")
}

func runServiceCtl(action string) {
	var cmd *exec.Cmd
	switch action {
	case "stop":
		// bootout unloads so KeepAlive does not relaunch — matches `systemctl stop`.
		cmd = exec.Command("launchctl", "bootout", launchdTarget())
	case "restart":
		// SIGTERM lets the daemon drain in-flight work (its signal handler
		// turns it into a graceful drain); KeepAlive then relaunches it. This
		// matches `systemctl --user restart`, which also sends SIGTERM.
		cmd = exec.Command("launchctl", "kill", "SIGTERM", launchdTarget())
	default:
		cmd = exec.Command("launchctl", action, launchdTarget())
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

func runStatus() {
	cmd := exec.Command("launchctl", "print", launchdTarget())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}
