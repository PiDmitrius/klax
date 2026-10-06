package timing

import (
	"testing"
	"time"
)

func TestRetryPacing(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{{-1, 625 * time.Millisecond}, {0, 625 * time.Millisecond}, {1, 1250 * time.Millisecond},
		{2, 2500 * time.Millisecond}, {3, 5 * time.Second}, {4, 5 * time.Second}, {1 << 30, 5 * time.Second}} {
		if got := RetryDelay(tc.attempt); got != tc.want {
			t.Errorf("attempt %d: %v, want %v", tc.attempt, got, tc.want)
		}
	}
}
