// Package pathutil provides path display helpers shared across klax packages.
package pathutil

import (
	"os"
	"strings"
)

// IsPathBoundaryByte returns true if b is a character that can border a path.
func IsPathBoundaryByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '"', '\'', '`', '(', ')', '[', ']', '{', '}', '<', '>', ':', ';', ',', '=', '!':
		return true
	default:
		return false
	}
}

// TildePathsInText replaces $HOME path prefixes inside arbitrary text with ~.
// Unlike a simple prefix check, it works when the path appears inside quotes,
// code spans, logs, or other surrounding text.
func TildePathsInText(s string) string {
	home, _ := os.UserHomeDir()
	if home == "" || s == "" {
		return s
	}

	var out strings.Builder
	i := 0
	for i < len(s) {
		idx := strings.Index(s[i:], home)
		if idx == -1 {
			out.WriteString(s[i:])
			break
		}
		idx += i
		end := idx + len(home)

		beforeOK := idx == 0 || IsPathBoundaryByte(s[idx-1])
		afterOK := end == len(s) || s[end] == '/' || IsPathBoundaryByte(s[end])
		if !beforeOK || !afterOK {
			out.WriteString(s[i:end])
			i = end
			continue
		}

		out.WriteString(s[i:idx])
		out.WriteByte('~')
		i = end
	}
	return out.String()
}
