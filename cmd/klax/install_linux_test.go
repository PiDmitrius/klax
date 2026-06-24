//go:build linux

package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenderServiceUnitPlacesStartLimitInUnitSection(t *testing.T) {
	unit := renderServiceUnit("/home/test/.local/bin/klax")

	want := "[Unit]\nDescription=klax — AI messaging bridge\nAfter=network.target\nStartLimitBurst=3\nStartLimitIntervalSec=60\n\n[Service]\nType=simple\nExecStart=/home/test/.local/bin/klax start --foreground\nRestart=always\nRestartSec=5\n"
	if !strings.Contains(unit, "OOMPolicy=continue") {
		t.Fatalf("service unit must keep the daemon alive across member OOM kills:\n%s", unit)
	}
	if !strings.Contains(unit, want) {
		t.Fatalf("service unit missing expected structure:\n%s", unit)
	}
	if strings.Contains(unit, "[Service]\nType=simple\nExecStart=/home/test/.local/bin/klax start --foreground\nRestart=always\nRestartSec=5\nStartLimitBurst=3") {
		t.Fatalf("start limit settings must not be in [Service]:\n%s", unit)
	}
}

func TestIgnorableVerifyError(t *testing.T) {
	if !ignorableVerifyError(fmt.Errorf("SO_PASSCRED failed: Operation not permitted")) {
		t.Fatalf("expected sandbox verification error to be ignorable")
	}
	if ignorableVerifyError(fmt.Errorf("syntax error in unit")) {
		t.Fatalf("real verification errors must stay fatal")
	}
}
