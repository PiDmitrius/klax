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
	if got := s.Active("user:test"); got == nil || got.KlaxID != first.KlaxID {
		t.Fatal("active session was not restored")
	}
	if len(s.SessionsFor("user:test")) != 1 || s.Scope["user:test"].Backend != "codex" {
		t.Fatal("failed creation changed store")
	}
	if _, err := os.Stat(filepath.Join(StoreDir(), "sessions.json.saved")); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteByIDUsesIdentityAfterOrderChanges(t *testing.T) {
	t.Setenv("KLAX_DATA_DIR", t.TempDir())
	s, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	first := s.New("user:test", "first", "/work", ScopeDefaults{})
	second := s.Add("user:test", &Session{Name: "second"})
	s.Reorder("user:test", []string{second.KlaxID, first.KlaxID})
	if !s.DeleteByID("user:test", first.KlaxID) {
		t.Fatal("target not deleted")
	}
	if s.Get("user:test", second.KlaxID) == nil || s.Get("user:test", first.KlaxID) != nil {
		t.Fatal("wrong identity deleted")
	}
}
