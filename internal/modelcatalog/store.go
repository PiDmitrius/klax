package modelcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

var ErrUpdating = errors.New("model catalog update already running")

// Store publishes a backend's catalog only after the complete response is saved.
type Store struct {
	mu       sync.Mutex
	path     string
	models   map[string][]Model
	updating map[string]bool
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, models: map[string][]Model{}, updating: map[string]bool{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &s.models); err != nil {
		return nil, err
	}
	if s.models == nil {
		s.models = map[string][]Model{}
	}
	for _, models := range s.models {
		if err = validate(models); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Models(backend string) []Model {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneModels(s.models[backend])
}

func (s *Store) Refresh(ctx context.Context, backend string) ([]Model, error) {
	return s.refresh(ctx, backend, Fetch)
}

func (s *Store) refresh(ctx context.Context, backend string, fetch func(context.Context, string) ([]Model, error)) ([]Model, error) {
	s.mu.Lock()
	if s.updating[backend] {
		s.mu.Unlock()
		return nil, ErrUpdating
	}
	s.updating[backend] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.updating, backend); s.mu.Unlock() }()
	models, err := fetch(ctx, backend)
	if err != nil {
		return nil, err
	}
	if err = validate(models); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string][]Model, len(s.models)+1)
	for key, list := range s.models {
		next[key] = list
	}
	next[backend] = cloneModels(models)
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".models-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return nil, err
	}
	s.models = next
	return models, nil
}

func cloneModels(models []Model) []Model {
	out := slices.Clone(models)
	for i := range out {
		out[i].Efforts = slices.Clone(out[i].Efforts)
	}
	return out
}
