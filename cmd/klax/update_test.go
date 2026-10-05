package main

import "testing"

// A release older than the data format would drop session ids on its first save.
func TestOlderReleasesDoNotReadCurrentData(t *testing.T) {
	for tag, want := range map[string]bool{"v0.8.10": false, "v0.7.453": false, "v0.9.0": true, "v0.9.12": true, "v1.0.0": true} {
		if got := readsCurrentData(tag); got != want {
			t.Fatalf("readsCurrentData(%s) = %v, want %v", tag, got, want)
		}
	}
}
