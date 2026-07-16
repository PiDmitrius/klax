package tg

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
