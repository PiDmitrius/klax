//go:build darwin

package main

import (
	"strings"
	"testing"
)

func TestRenderLaunchAgentStructure(t *testing.T) {
	plist := renderLaunchAgent("/Users/test/.local/bin/klax", "/opt/homebrew/bin:/usr/bin", "/Users/test/Library/Logs/klax.log")

	for _, want := range []string{
		"<key>Label</key>\n\t<string>klax</string>",
		"<string>/Users/test/.local/bin/klax</string>",
		"<string>start</string>",
		"<string>--foreground</string>",
		// KeepAlive mirrors systemd Restart=always so drain-exit on update relaunches.
		"<key>KeepAlive</key>\n\t<true/>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>ThrottleInterval</key>\n\t<integer>5</integer>",
		// PATH must be seeded so the backend CLIs resolve under launchd's minimal env.
		"<key>PATH</key>\n\t\t<string>/opt/homebrew/bin:/usr/bin</string>",
		"<string>/Users/test/Library/Logs/klax.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("LaunchAgent plist missing %q:\n%s", want, plist)
		}
	}
}

func TestLaunchdPathEnvSeedsBackendDirs(t *testing.T) {
	p := launchdPathEnv()
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/.local/bin"} {
		if !strings.Contains(p, dir) {
			t.Fatalf("launchd PATH missing %q: %s", dir, p)
		}
	}
}
