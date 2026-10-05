package modelcatalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRefreshDurableAndFailuresPreserveCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	load := func(models []Model) func(context.Context, string) ([]Model, error) {
		return func(context.Context, string) ([]Model, error) { return models, nil }
	}
	original := []Model{{Value: "old", Label: "Old", Default: true, ThinkLevels: []string{"high", "max"}}}
	for _, backend := range []string{"codex", "claude"} {
		if _, err = s.refresh(context.Background(), backend, load(original)); err != nil {
			t.Fatal(err)
		}
	}
	diskBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	failures := []func(context.Context, string) ([]Model, error){
		func(context.Context, string) ([]Model, error) { return nil, errors.New("offline") },
		load(nil), load([]Model{{Value: "missing-label", Label: ""}}), load([]Model{{Value: "same", Label: "One"}, {Value: "same", Label: "Two"}}),
	}
	for _, fetch := range failures {
		if _, err = s.refresh(context.Background(), "codex", fetch); err == nil {
			t.Fatal("invalid update succeeded")
		}
		diskAfter, _ := os.ReadFile(path)
		if string(diskAfter) != string(diskBefore) || !reflect.DeepEqual(s.Models("codex"), original) {
			t.Fatal("failed update replaced catalog")
		}
	}
	s.path = filepath.Join(path, "impossible")
	if _, err = s.refresh(context.Background(), "codex", load([]Model{{Value: "new", Label: "New"}})); err == nil {
		t.Fatal("save unexpectedly succeeded")
	}
	if !reflect.DeepEqual(s.Models("codex"), original) {
		t.Fatal("failed save published models")
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Models("codex"), original) || !reflect.DeepEqual(s.Models("claude"), original) {
		t.Fatal("restart lost catalogs")
	}
	copy := s.Models("codex")
	copy[0].Value = "changed"
	copy[0].ThinkLevels[0] = "changed"
	if s.Models("codex")[0].ThinkLevels[0] != "high" {
		t.Fatal("caller mutated efforts")
	}
	if s.Models("codex")[0].Value != "old" {
		t.Fatal("caller mutated catalog")
	}
}

func TestRefreshSerializesPerBackendWithoutBlockingReaders(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := s.refresh(context.Background(), "codex", func(context.Context, string) ([]Model, error) {
			close(entered)
			<-release
			return []Model{{Value: "code", Label: "Code"}}, nil
		})
		done <- err
	}()
	<-entered
	fetch := func(context.Context, string) ([]Model, error) { return []Model{{Value: "chat", Label: "Chat"}}, nil }
	if _, err := s.refresh(context.Background(), "codex", fetch); !errors.Is(err, ErrUpdating) {
		t.Fatal(err)
	}
	if s.Models("codex") != nil {
		t.Fatal("published before completion")
	}
	if _, err := s.refresh(context.Background(), "claude", fetch); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Models("claude")[0].Value != "chat" || reloaded.Models("codex")[0].Value != "code" {
		t.Fatal("parallel save lost a backend")
	}
}

func TestOpenAdoptsEffortsAsThinkLevels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"codex":[{"value":"gpt-a","label":"A","efforts":["low"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Models("codex"); len(got) != 1 || len(got[0].ThinkLevels) != 1 || got[0].ThinkLevels[0] != "low" {
		t.Fatalf("efforts must become think_levels, got %+v", got)
	}
}
