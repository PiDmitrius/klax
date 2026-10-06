package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDownloadBodyInactivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := download(srv.Client(), req, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled body: %v", err)
	}
}

func TestDownloadProgressRenewsBudget(t *testing.T) {
	next := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 4 {
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			select {
			case <-next:
			case <-r.Context().Done():
				return
			}
		}
	}))
	defer srv.Close()
	client := srv.Client()
	client.Timeout = time.Millisecond
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := download(client, req, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 1)
	for range 4 {
		if n, err := resp.Body.Read(buf); n != 1 || err != nil {
			t.Fatalf("progressing body: n=%d err=%v", n, err)
		}
		time.Sleep(100 * time.Millisecond)
		select {
		case next <- struct{}{}:
		case <-time.After(time.Second):
			t.Fatal("download stopped before completing")
		}
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if client.Timeout != time.Millisecond {
		t.Fatal("download changed the ordinary request budget")
	}
}

func TestDownloadHeadersInactivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := download(srv.Client(), req, 100*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled headers: %v", err)
	}
}
