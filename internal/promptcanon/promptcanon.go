// Package promptcanon owns the canonical representation used to correlate a
// submitted prompt with the backend transcript record that contains it.
package promptcanon

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Canonical preserves prompt content except for newline spelling. Both backend
// transports serialize CRLF/CR input as LF in their transcript payloads.
func Canonical(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

func Sum(text string) [32]byte { return sha256.Sum256([]byte(Canonical(text))) }

func Digest(text string) string {
	s := Sum(text)
	return hex.EncodeToString(s[:])
}
