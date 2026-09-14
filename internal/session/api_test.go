package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddPersistedFailureRestoresSessionAndDefaults(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	first := s.New("user:test", "first", "/work", ScopeDefaults{Backend: "codex"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	before := s.HighWater
	if err := os.Rename(s.path, s.path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.path, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = s.AddPersisted("user:test", &Session{Name: "uncommitted"}, &ScopeDefaults{Backend: "claude"})
	if err == nil {
		t.Fatal("expected save failure")
	}
	if got := s.Active("user:test"); got == nil || got.Created != first.Created {
		t.Fatal("active session was not restored")
	}
	if s.HighWater != before || len(s.SessionsFor("user:test")) != 1 || s.Scope["user:test"].Backend != "codex" {
		t.Fatal("failed creation changed store")
	}
	if _, err := os.Stat(filepath.Join(StoreDir(), "sessions.json.saved")); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteCreatedUsesIdentityAfterOrderChanges(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	first := s.New("user:test", "first", "/work", ScopeDefaults{})
	second := s.Add("user:test", &Session{Name: "second"})
	s.Reorder("user:test", []int64{second.Created, first.Created})
	if !s.DeleteCreated("user:test", first.Created) {
		t.Fatal("target not deleted")
	}
	if s.Get("user:test", second.Created) == nil || s.Get("user:test", first.Created) != nil {
		t.Fatal("wrong identity deleted")
	}
}
