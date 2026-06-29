//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
)

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
