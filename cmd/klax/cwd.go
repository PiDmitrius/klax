package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func resolveWorkingDir(raw string) (string, error) {
	cwd := strings.TrimSpace(raw)
	if cwd == "" {
		return "", fmt.Errorf("рабочий каталог не может быть пустым")
	}
	home, _ := os.UserHomeDir()
	cwd = filepath.Clean(expandPathValue(cwd, home))
	fi, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("рабочий каталог недоступен: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("рабочий каталог не является каталогом")
	}
	return cwd, nil
}
