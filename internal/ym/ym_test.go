package ym

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PiDmitrius/klax/internal/transport"
)

// newTestBot starts an httptest.Server, points the package-level apiBase at
// it (restored via t.Cleanup), and returns a Bot wired to it.
func newTestBot(t *testing.T, handler http.HandlerFunc) *Bot {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	origBase := apiBase
	apiBase = ts.URL + "/bot/v1/"
	t.Cleanup(func() { apiBase = origBase })

	b := New("test-token")
	b.client = ts.Client()
	return b
}

func TestGetMeSendsOAuthHeaderAndParsesInfo(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	b := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"id":"bot1","login":"klax@example.org","display_name":"klax"}`))
	})

	info, err := b.GetMe()
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if gotAuth != "OAuth test-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "OAuth test-token")
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/bot/v1/self/get" {
		t.Errorf("path = %q", gotPath)
	}
	if info.ID != "bot1" || info.Login != "klax@example.org" {
		t.Errorf("info = %+v", info)
	}
}

func TestSendMsgAddressesLoginVsChatID(t *testing.T) {
	var gotBody map[string]interface{}
	b := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"message_id":42}`))
	})

	// Private chat: addressed by login.
	if _, err := b.SendMessageReturnID("vasya@example.org", "hi", "", ""); err != nil {
		t.Fatalf("send to login: %v", err)
	}
	if gotBody["login"] != "vasya@example.org" {
		t.Errorf("expected login field, got %+v", gotBody)
	}
	if _, ok := gotBody["chat_id"]; ok {
		t.Errorf("chat_id should not be set for a login address, got %+v", gotBody)
	}

	// Group/channel chat: addressed by chat_id.
	if _, err := b.SendMessageReturnID("0/0/guid", "hi", "", ""); err != nil {
		t.Fatalf("send to chat_id: %v", err)
	}
	if gotBody["chat_id"] != "0/0/guid" {
		t.Errorf("expected chat_id field, got %+v", gotBody)
	}
}

func TestEditMessageSetsMessageID(t *testing.T) {
	var gotBody map[string]interface{}
	b := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"message_id":42}`))
	})

	if err := b.EditMessage("vasya@example.org", "1647523230504005", "updated", ""); err != nil {
		t.Fatalf("EditMessage: %v", err)
	}
	if gotBody["message_id"] != float64(1647523230504005) {
		t.Errorf("message_id = %v, want 1647523230504005", gotBody["message_id"])
	}
	if gotBody["text"] != "updated" {
		t.Errorf("text = %v", gotBody["text"])
	}
}

func TestGetUpdatesAdvancesOffset(t *testing.T) {
	calls := 0
	b := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"ok":true,"updates":[{"update_id":5},{"update_id":7}]}`))
		} else {
			_, _ = w.Write([]byte(`{"ok":true,"updates":[]}`))
		}
	})

	updates, err := b.GetUpdates()
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("len(updates) = %d, want 2", len(updates))
	}
	if b.offset != 8 {
		t.Errorf("offset = %d, want 8 (last update_id 7 + 1)", b.offset)
	}

	if _, err := b.GetUpdates(); err != nil {
		t.Fatalf("second GetUpdates: %v", err)
	}
	if b.offset != 8 {
		t.Errorf("offset changed on empty response: %d", b.offset)
	}
}

func TestAPIErrorSurfacesPlatformAndCode(t *testing.T) {
	b := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bot is not a member of the chat"}`))
	})

	_, err := b.SendMessageReturnID("vasya@example.org", "hi", "", "")
	var apiErr *transport.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *transport.APIError, got %v (%T)", err, err)
	}
	if apiErr.Platform != "ym" {
		t.Errorf("Platform = %q, want ym", apiErr.Platform)
	}
	if apiErr.Code != http.StatusForbidden {
		t.Errorf("Code = %d, want 403", apiErr.Code)
	}
}

func TestDownloadFileReturnsBytesOrError(t *testing.T) {
	b := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("file_id") == "missing" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":false,"description":"Failed to get file"}`))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("binary-data"))
	})

	data, err := b.DownloadFile("file123")
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if string(data) != "binary-data" {
		t.Errorf("data = %q", data)
	}

	_, err = b.DownloadFile("missing")
	var apiErr *transport.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *transport.APIError, got %v", err)
	}
}

func TestIsLoginAndIsGroup(t *testing.T) {
	cases := []struct {
		raw       string
		wantLogin bool
	}{
		{"vasya@example.org", true},
		{"0/0/4f24b544-697c-4e18-a9c1-b39432ee9bf9", false},
	}
	for _, c := range cases {
		if got := IsLogin(c.raw); got != c.wantLogin {
			t.Errorf("IsLogin(%q) = %v, want %v", c.raw, got, c.wantLogin)
		}
		if got := IsGroup(c.raw); got == c.wantLogin {
			t.Errorf("IsGroup(%q) = %v, want %v", c.raw, got, !c.wantLogin)
		}
	}
}

func TestBestImage(t *testing.T) {
	if BestImage(nil) != nil {
		t.Error("BestImage(nil) should be nil")
	}
	variants := []Image{
		{FileID: "small", Size: 100},
		{FileID: "large", Size: 900},
		{FileID: "medium", Size: 400},
	}
	best := BestImage(variants)
	if best == nil || best.FileID != "large" {
		t.Errorf("BestImage = %+v, want large", best)
	}
}

func TestBestImageFallsBackToAreaWhenSizeMissing(t *testing.T) {
	// Per docs, `size` is only populated for the original — every thumbnail
	// variant here has Size==0, so area (width*height) must decide.
	variants := []Image{
		{FileID: "thumb", Width: 100, Height: 100},
		{FileID: "original", Width: 1920, Height: 1080},
		{FileID: "mid", Width: 640, Height: 480},
	}
	best := BestImage(variants)
	if best == nil || best.FileID != "original" {
		t.Errorf("BestImage = %+v, want original (largest area)", best)
	}
}

func TestGetUpdatesUsesMaxUpdateIDNotLastElement(t *testing.T) {
	b := newTestBot(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Deliberately unsorted: the last element is NOT the max update_id.
		_, _ = w.Write([]byte(`{"ok":true,"updates":[{"update_id":7},{"update_id":9},{"update_id":5}]}`))
	})

	if _, err := b.GetUpdates(); err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if b.offset != 10 {
		t.Errorf("offset = %d, want 10 (max update_id 9 + 1)", b.offset)
	}
}
