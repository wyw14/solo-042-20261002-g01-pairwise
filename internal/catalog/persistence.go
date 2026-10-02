package catalog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// FileStore persists the device catalog as a single JSON document.
// Writes go through a temporary file and an atomic rename so a crash can
// never leave a torn catalog behind.
type FileStore struct {
	mu   sync.Mutex
	path string
}

func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// Save atomically replaces the catalog document on disk.
func (s *FileStore) Save(config Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}

// Load reads the catalog document. The boolean result is false when no
// catalog has been persisted yet.
func (s *FileStore) Load() (Config, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, false, err
	}
	return config, true, nil
}
