package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUISendTestCycle(t *testing.T) {
	f := newAPIFixture(t, "", "", "")
	f.s.sendTest.enabled = true
	sr := f.d.getRunner("user:test", f.created)
	for cycle := 0; cycle < 2; cycle++ {
		nonce := fmt.Sprintf("test-cycle-%d", cycle)
		if w := f.send("queued", nonce); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "1/3") {
			t.Fatalf("fast rejection: %d %s", w.Code, w.Body.String())
		}
		ctx, cancel := context.WithCancel(context.Background())
		r := httptest.NewRequest(http.MethodPost, "/api/send", strings.NewReader(fmt.Sprintf(`{"session":%d,"text":"hello","nonce":%q}`, f.created, nonce))).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer access")
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.s.routes().ServeHTTP(w, r)
		}()
		select {
		case <-done:
			cancel()
			t.Fatal("hung attempt returned before client cancellation")
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("hung attempt did not stop on cancellation")
		}
		if w.Flushed || w.Body.Len() != 0 {
			t.Fatal("hung attempt wrote a response")
		}
		turns, err := sr.store.InboundLog()
		if err != nil || len(turns) != cycle {
			t.Fatalf("injected failure enqueued a message: count=%d err=%v", len(turns), err)
		}
		if w := f.send("queued", nonce); w.Code != http.StatusNoContent {
			t.Fatalf("third attempt: %d %s", w.Code, w.Body.String())
		}
		turns, err = sr.store.InboundLog()
		if err != nil || len(turns) != cycle+1 {
			t.Fatalf("third attempt not durable: count=%d err=%v", len(turns), err)
		}
	}
}

func TestUISendTestDisabledAndUserIsolation(t *testing.T) {
	var gate uiSendTest
	r := httptest.NewRequest(http.MethodPost, "/api/send", nil)
	t.Setenv("KLAX_UI_SEND_TEST", "1")
	for i := 0; i < 3; i++ {
		if gate.intercept(httptest.NewRecorder(), r, "first") {
			t.Fatal("disabled test mode intercepted a send")
		}
	}
	gate.enabled = true
	for _, user := range []string{"first", "second"} {
		w := httptest.NewRecorder()
		if !gate.intercept(w, r, user) || w.Code != http.StatusServiceUnavailable {
			t.Fatal("each user must start with a fast rejection")
		}
	}
}
