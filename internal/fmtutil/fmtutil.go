// Package fmtutil provides shared formatting helpers.
package fmtutil

import (
	"fmt"
	"time"
)

// Duration formats a duration as a compact Russian string: "3д5ч", "2ч15м", "42м".
// Durations under one minute are shown as "менее минуты".
func Duration(d time.Duration) string {
	if d < time.Minute {
		return "менее минуты"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		if hours > 0 {
			return fmt.Sprintf("%dд%dч", days, hours)
		}
		return fmt.Sprintf("%dд", days)
	}
	if hours > 0 {
		return fmt.Sprintf("%dч%dм", hours, mins)
	}
	return fmt.Sprintf("%dм", mins)
}
