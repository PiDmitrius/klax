package tg

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		body := `{"ok":true,"result":{"id":1}}`
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			var payload map[string]int
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			want := 29
			if calls == 1 {
				want = 0
			}
			if payload["timeout"] != want {
				t.Fatalf("call %d timeout = %d, want %d", calls, payload["timeout"], want)
			}
			body = `{"ok":true,"result":[]}`
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

// newTestBot starts an httptest.Server, points the package-level apiBase at
// it (restored via t.Cleanup), and returns a Bot wired to it.
func newTestBot(t *testing.T, handler http.HandlerFunc) *Bot {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	origBase := apiBase
	apiBase = ts.URL + "/bot"
	t.Cleanup(func() { apiBase = origBase })

	b := New("test-token")
	b.client = ts.Client()
	return b
}

func TestGetMeDecodesIdentity(t *testing.T) {
	var gotPath string
	b := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":123456,"is_bot":true,"first_name":"klax","username":"klax_dev_bot"}}`))
	})

	me, err := b.GetMe()
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if me.ID != 123456 || me.Username != "klax_dev_bot" {
		t.Fatalf("GetMe = %+v, want id=123456 username=klax_dev_bot", me)
	}
	if gotPath != "/bottest-token/getMe" {
		t.Fatalf("request path = %q, want /bottest-token/getMe", gotPath)
	}
}

func TestGetMeReturnsAPIErrorWhenNotOK(t *testing.T) {
	b := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	})

	_, err := b.GetMe()

	if err == nil {
		t.Fatal("expected an error for ok=false")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Code != 401 {
		t.Fatalf("err = %v, want *APIError with code 401", err)
	}
}
