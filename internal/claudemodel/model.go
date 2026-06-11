// Package claudemodel centralizes how klax maps a Claude model alias to the
// --model value it launches claude with, and the context window that value
// implies. It exists so the -p backend and the interactive tty driver apply
// one rule instead of each guessing — the two disagreeing is what let a
// resumed session silently compact at 200k while the gauge claimed 1M.
package claudemodel

import "strings"

// Normalize maps a stored model alias to the --model value klax launches.
//
// The bare "fable" alias resolves in the CLI to claude-fable-5, but its
// believed context window is 1M only when the auth lane is first-party;
// otherwise it silently falls back to 200k, and the interactive TUI then
// compacts a resumed session the moment it crosses that — observed live at
// 206k–212k. The "[1m]" marker forces the CLI's 1M window unconditionally,
// which is the behavior klax's "Claude Fable 1M" offering promises, so a bare
// "fable" is upgraded to "fable[1m]". The mapping is idempotent and leaves
// every other model string untouched.
func Normalize(model string) string {
	if strings.EqualFold(strings.TrimSpace(model), "fable") {
		return "fable[1m]"
	}
	return model
}

// ContextWindow returns the believed context window, in tokens, for a launched
// --model value. The interactive CLI never reports this number and the
// transcript never carries it, so klax estimates it to drive its context
// gauge. A "[1m]" variant (including the normalized bare "fable") is 1M; every
// other model runs the standard 200k.
func ContextWindow(model string) int {
	if strings.Contains(Normalize(model), "[1m]") {
		return 1_000_000
	}
	return 200_000
}
