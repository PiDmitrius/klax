package ids

import (
	"regexp"
	"testing"
)

func TestNewIsDistinctAlphanumeric(t *testing.T) {
	shape := regexp.MustCompile(`^[A-Za-z0-9]+$`)
	seen := map[string]bool{}
	for range 1000 {
		id := New()
		if len(id) != Length || !shape.MatchString(id) || seen[id] {
			t.Fatalf("New() = %q", id)
		}
		seen[id] = true
	}
}
