package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/PiDmitrius/klax/internal/session"
)

// --- Service control ---

func runServiceStart() {
	cmd := exec.Command("systemctl", "--user", "start", "klax")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "failed: %v\nTry 'klax install' first, or 'klax start --foreground'\n", err)
		os.Exit(1)
	}
	fmt.Println("klax started")
}

func runServiceCtl(action string) {
	cmd := exec.Command("systemctl", "--user", action, "klax")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

func runStatus() {
	cmd := exec.Command("systemctl", "--user", "status", "klax", "--no-pager", "-l")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

// --- Restart marker ---

type restartMarker struct {
	Reason    string `json:"reason"`    // "update" or "restart"
	Version   string `json:"version"`   // version before restart
	Timestamp int64  `json:"timestamp"` // unix timestamp
}

func markerPath() string {
	return filepath.Join(session.StoreDir(), "restart.marker")
}

func writeMarker(reason string) error {
	m := restartMarker{
		Reason:    reason,
		Version:   version,
		Timestamp: time.Now().Unix(),
	}
	if err := os.MkdirAll(session.StoreDir(), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(markerPath(), data, 0600)
}

func readMarker() *restartMarker {
	data, err := os.ReadFile(markerPath())
	if err != nil {
		return nil
	}
	var m restartMarker
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return &m
}

func removeMarker() {
	os.Remove(markerPath())
}

// --- PID file ---

func pidPath() string {
	return filepath.Join(session.StoreDir(), "klax.pid")
}

func writePID() {
	os.MkdirAll(session.StoreDir(), 0700)
	os.WriteFile(pidPath(), []byte(fmt.Sprintf("%d", os.Getpid())), 0600)
}

func removePID() {
	os.Remove(pidPath())
}
