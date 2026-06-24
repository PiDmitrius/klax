//go:build darwin

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// launchdLabel is the launchd service label and the basename of the plist.
const launchdLabel = "klax"

// launchAgentPath is the per-user LaunchAgent plist location.
func launchAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

// launchdDomain is the per-user GUI domain (gui/<uid>) launchd commands target.
func launchdDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// launchdTarget is the fully-qualified service target (gui/<uid>/klax).
func launchdTarget() string {
	return launchdDomain() + "/" + launchdLabel
}

// launchdLogPath is where the daemon's stdout/stderr are written, mirroring the
// journal that systemd provides on Linux.
func launchdLogPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Logs", launchdLabel+".log")
}

// launchdPathEnv builds the PATH the daemon runs with. launchd, like a systemd
// user service, hands the process a minimal PATH, so the backend CLIs (claude
// in ~/.local/bin, codex from Homebrew) would not resolve. We seed the common
// Homebrew prefixes plus ~/.local/bin explicitly.
func launchdPathEnv() string {
	home, _ := os.UserHomeDir()
	return strings.Join([]string{
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
		filepath.Join(home, ".local", "bin"),
	}, ":")
}

func runInstall() {
	dst := installBinary()

	plistPath := launchAgentPath()
	os.MkdirAll(filepath.Dir(plistPath), 0755)
	os.MkdirAll(filepath.Dir(launchdLogPath()), 0755)

	plist := renderLaunchAgent(dst, launchdPathEnv(), launchdLogPath())
	drifted, err := unitDrifted(plistPath, plist)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot inspect current LaunchAgent: %v\n", err)
		os.Exit(1)
	} else if drifted {
		fmt.Fprintf(os.Stderr, "warning: local LaunchAgent drift detected, overwriting %s\n", tildePath(plistPath))
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "cannot install LaunchAgent: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("installed: %s\n", tildePath(plistPath))

	// Write restart marker if not already present (update writes it before build).
	if readMarker() == nil {
		if err := writeMarker("update"); err != nil {
			log.Printf("warning: could not write restart marker: %v", err)
		}
	}

	// If the daemon is already running (an update over a live service), do NOT
	// bootout it — that would hard-kill the in-flight turn. Just like the
	// systemd path, install never stops the running service: the daemon watches
	// the restart marker, drains gracefully, exits, and launchd's KeepAlive
	// relaunches it from the freshly written binary (the ProgramArguments path
	// is constant). A changed plist (rare; only the binary path is templated)
	// needs a full reload, so warn when it drifts while loaded.
	if serviceLoaded() {
		if drifted {
			fmt.Println("\nNote: LaunchAgent changed; run 'klax stop && klax start' to apply it.")
		}
		fmt.Println("\nInstalled. Service is running — it will restart automatically.")
		return
	}

	// Fresh load: enable (in case it was previously disabled) then bootstrap;
	// RunAtLoad starts it. Bootstrap can legitimately fail when there is no GUI
	// session for gui/<uid> (e.g. a plain SSH shell), so surface real errors
	// instead of claiming success.
	exec.Command("launchctl", "enable", launchdTarget()).Run()
	if out, err := exec.Command("launchctl", "bootstrap", launchdDomain(), plistPath).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			fmt.Fprintf(os.Stderr, "warning: could not load service: %s\n", msg)
		}
		fmt.Println("\nInstalled. Run: klax start (or 'klax start --foreground' to run directly)")
		return
	}
	fmt.Println("\nInstalled. Service is running — it will restart automatically.")
}

// serviceLoaded reports whether the LaunchAgent is currently bootstrapped into
// the user's launchd domain.
func serviceLoaded() bool {
	return exec.Command("launchctl", "print", launchdTarget()).Run() == nil
}

func runUninstall() {
	exec.Command("launchctl", "bootout", launchdTarget()).Run()
	os.Remove(launchAgentPath())
	fmt.Println("uninstalled")
}

// renderLaunchAgent builds the LaunchAgent plist. KeepAlive mirrors systemd's
// Restart=always (so a drain-exit on update is relaunched with the new
// binary); ThrottleInterval mirrors RestartSec=5.
func renderLaunchAgent(binPath, pathEnv, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>start</string>
		<string>--foreground</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>%s</string>
	</dict>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, launchdLabel, binPath, pathEnv, logPath, logPath)
}
