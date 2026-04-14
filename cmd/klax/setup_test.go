package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestPromptValidatedTokenKeepKeepsCurrentOnEmptyInput(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("\n"))

	got := promptValidatedTokenKeep(reader, "Token", "current-token", func(string) error { return nil })

	if got != "current-token" {
		t.Fatalf("expected current token to be kept, got %q", got)
	}
}

func TestPromptValidatedTokenKeepClearsOnDash(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("-\n"))

	got := promptValidatedTokenKeep(reader, "Token", "current-token", func(string) error { return nil })

	if got != "" {
		t.Fatalf("expected token to be cleared, got %q", got)
	}
}

func TestPromptInt64ListKeepKeepsCurrentOnEmptyInput(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("\n"))

	got := promptInt64ListKeep(reader, "Users", []int64{1, 2, 3})

	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("expected current values to be kept, got %v", got)
	}
}

func TestPromptInt64ListKeepClearsOnDash(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("-\n"))

	got := promptInt64ListKeep(reader, "Users", []int64{1, 2, 3})

	if len(got) != 0 {
		t.Fatalf("expected list to be cleared, got %v", got)
	}
}

func TestPromptInt64ListKeepParsesReplacement(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("10, 20\n"))

	got := promptInt64ListKeep(reader, "Users", []int64{1, 2, 3})

	if len(got) != 2 || got[0] != 10 || got[1] != 20 {
		t.Fatalf("expected replacement values, got %v", got)
	}
}

func TestPromptStringKeepSupportsKeepAndClear(t *testing.T) {
	keepReader := bufio.NewReader(strings.NewReader("\n"))
	if got := promptStringKeep(keepReader, "Path", "~/work"); got != "~/work" {
		t.Fatalf("expected current string to be kept, got %q", got)
	}

	clearReader := bufio.NewReader(strings.NewReader("-\n"))
	if got := promptStringKeep(clearReader, "Path", "~/work"); got != "" {
		t.Fatalf("expected string to be cleared, got %q", got)
	}
}

func TestExpandPathValueExpandsHomeDisplayPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	tests := []struct {
		in   string
		want string
	}{
		{in: "~", want: home},
		{in: "~/work", want: home + "/work"},
		{in: "/tmp/work", want: "/tmp/work"},
		{in: "", want: ""},
	}

	for _, tt := range tests {
		if got := expandPathValue(tt.in, home); got != tt.want {
			t.Fatalf("expandPathValue(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
