package max

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type pollTransport func(*http.Request) (*http.Response, error)

func (f pollTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPollBudgetAndDrain(t *testing.T) {
	b := New("test-token")
	budgets := []time.Duration{30 * time.Second, 10 * time.Second, 10 * time.Second}
	calls := 0
	b.client.Transport = pollTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) < budgets[calls]-time.Second || time.Until(deadline) > budgets[calls] {
			t.Fatalf("call %d has an unexpected deadline: %v", calls, deadline)
		}
		if r.Header.Get("Authorization") != "test-token" {
			t.Fatal("missing authorization")
		}
		body := `{"user_id":1}`
		if r.URL.Path == "/updates" {
			want := "29"
			if calls == 1 {
				want = "0"
			}
			if r.URL.Query().Get("timeout") != want {
				t.Fatalf("call %d query = %s", calls, r.URL.RawQuery)
			}
			body = `{"updates":[],"marker":1}`
		}
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if _, err := b.GetUpdates(); err != nil {
		t.Fatal(err)
	}
	if err := b.DrainUpdates(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetMe(); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}
