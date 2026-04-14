package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestRenderServiceUnitPlacesStartLimitInUnitSection(t *testing.T) {
	unit := renderServiceUnit("/home/test/.local/bin/klax")

	want := "[Unit]\nDescription=klax — AI messaging bridge\nAfter=network.target\nStartLimitBurst=3\nStartLimitIntervalSec=60\n\n[Service]\nType=simple\nExecStart=/home/test/.local/bin/klax start --foreground\nRestart=always\nRestartSec=5\n"
	if !strings.Contains(unit, want) {
		t.Fatalf("service unit missing expected structure:\n%s", unit)
	}
	if strings.Contains(unit, "[Service]\nType=simple\nExecStart=/home/test/.local/bin/klax start --foreground\nRestart=always\nRestartSec=5\nStartLimitBurst=3") {
		t.Fatalf("start limit settings must not be in [Service]:\n%s", unit)
	}
}

func TestUnitDriftedDetectsDifferentContent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/klax.service"
	if err := os.WriteFile(path, []byte("broken"), 0644); err != nil {
		t.Fatalf("write unit: %v", err)
	}

	drifted, err := unitDrifted(path, "expected")
	if err != nil {
		t.Fatalf("unitDrifted error: %v", err)
	}
	if !drifted {
		t.Fatalf("expected drift to be detected")
	}
}

func TestUnitDriftedIgnoresMissingFile(t *testing.T) {
	drifted, err := unitDrifted("/no/such/file", "expected")
	if err != nil {
		t.Fatalf("unitDrifted error: %v", err)
	}
	if drifted {
		t.Fatalf("missing file should not count as drift")
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
