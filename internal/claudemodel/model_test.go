package claudemodel

import "testing"

func TestNormalizeUpgradesBareFable(t *testing.T) {
	cases := map[string]string{
		"fable":     "fable[1m]", // the picker's stored value and the bug's source
		"Fable":     "fable[1m]", // case-insensitive
		" fable ":   "fable[1m]", // tolerate stray whitespace
		"fable[1m]": "fable[1m]", // idempotent — never doubles the marker
		"opus":      "opus",
		"opus[1m]":  "opus[1m]",
		"sonnet":    "sonnet",
		"haiku":     "haiku",
		// A raw model id is launched verbatim; only the alias carries the
		// 1M promise, so it is left untouched.
		"claude-fable-5": "claude-fable-5",
		"":               "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContextWindow(t *testing.T) {
	cases := map[string]int{
		"fable":      1_000_000, // normalized to fable[1m] first
		"fable[1m]":  1_000_000,
		"sonnet[1m]": 1_000_000,
		"opus[1m]":   1_000_000,
		"opus":       200_000,
		"sonnet":     200_000,
		"haiku":      200_000,
		// A raw bare id is what the CLI actually treats as 200k off the
		// first-party lane; report that honestly rather than assuming 1M.
		"claude-fable-5": 200_000,
		"":               200_000,
	}
	for model, want := range cases {
		if got := ContextWindow(model); got != want {
			t.Errorf("ContextWindow(%q) = %d, want %d", model, got, want)
		}
	}
}
