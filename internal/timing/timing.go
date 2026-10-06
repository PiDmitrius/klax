// Package timing owns network request budgets and retry pacing for the server and UI.
package timing

import "time"

const (
	RequestTimeout = 10 * time.Second
	WorkTimeout    = RequestTimeout * 3 / 4
	RetryMin       = RequestTimeout / 16
	RetryMax       = RequestTimeout / 2
	PollTimeout    = 30 * time.Second
	PollHold       = PollTimeout - time.Second
)

func RetryDelay(attempt int) time.Duration {
	d := RetryMin
	for i := 0; i < attempt && d < RetryMax; i++ {
		d *= 2
	}
	return min(d, RetryMax)
}
