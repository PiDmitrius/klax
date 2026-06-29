package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// installBinary copies the running executable to ~/.local/bin/klax and returns
// the destination path. Shared by the Linux (systemd) and macOS (launchd)
// install flows.
func installBinary() string {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find executable: %v\n", err)
		os.Exit(1)
	}

	home, _ := os.UserHomeDir()
	binDir := filepath.Join(home, ".local", "bin")
	os.MkdirAll(binDir, 0755)
	dst := filepath.Join(binDir, "klax")
	if err := copyFile(exe, dst, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot install: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("installed: %s\n", tildePath(dst))
	return dst
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

// unitDrifted reports whether the file at path differs from the expected
// content. A missing file is not drift. Used for both the systemd unit and the
// launchd plist.
func unitDrifted(path, expected string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return !bytes.Equal(data, []byte(expected)), nil
}
