package main

import (
	"os"
	"testing"
)

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
