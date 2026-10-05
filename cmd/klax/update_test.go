package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A release older than the data format would drop session ids on its first save.
func TestOlderReleasesDoNotReadCurrentData(t *testing.T) {
	for tag, want := range map[string]bool{"v0.8.10": false, "v0.7.453": false, "v0.9.0": true, "v0.9.12": true, "v1.0.0": true} {
		if got := readsCurrentData(tag); got != want {
			t.Fatalf("readsCurrentData(%s) = %v, want %v", tag, got, want)
		}
	}
}

type releaseDownloadProbe struct{ calls int }

func (p *releaseDownloadProbe) RoundTrip(*http.Request) (*http.Response, error) {
	p.calls++
	return nil, errors.New("download probe")
}

func TestReleaseUpdateRejectsOlderDataFormatBeforeDownload(t *testing.T) {
	probe := &releaseDownloadProbe{}
	prior := downloadClient
	downloadClient = &http.Client{Transport: probe}
	t.Cleanup(func() { downloadClient = prior })
	for _, tag := range []string{"v0.8.10", "v0.7.453"} {
		res := performReleaseUpdate(context.Background(), tag, io.Discard)
		if res.OK || !strings.Contains(res.Message, "cannot read the current data") {
			t.Fatalf("performReleaseUpdate(%q) = %+v", tag, res)
		}
	}
	if probe.calls != 0 {
		t.Fatalf("older releases reached the download path: %d requests", probe.calls)
	}
	res := performReleaseUpdate(context.Background(), "v0.9.0", io.Discard)
	if probe.calls != 1 || !strings.Contains(res.Message, "download probe") {
		t.Fatalf("a supported release did not reach the download path: %+v, requests=%d", res, probe.calls)
	}
}
