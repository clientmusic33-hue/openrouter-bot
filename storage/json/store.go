// Package json provides atomic JSON file-based persistence implementing storage.Store.
package json

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"openrouter-bot/internal/atomicfile"
	"openrouter-bot/internal/security"
)

// Store persists documents as atomic JSON files under BaseDir/<namespace>/<key>.json.
type Store struct {
	baseDir string
	mu      sync.RWMutex
}

// New creates a JSON file store rooted at baseDir.
func New(baseDir string) *Store {
	if strings.TrimSpace(baseDir) == "" {
		baseDir = "data"
	}
	return &Store{baseDir: baseDir}
}

func (s *Store) Type() string { return "json" }

func (s *Store) resolvePath(namespace, key string) (string, error) {
	ns := security.SanitizeFilename(namespace)
	k := security.SanitizeFilename(key)
	if !strings.HasSuffix(strings.ToLower(k), ".json") {
		k += ".json"
	}
	return security.SafePath(s.baseDir, filepath.Join(ns, k))
}

func (s *Store) Save(_ context.Context, namespace, key string, data []byte) error {
	path, err := s.resolvePath(namespace, key)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return atomicfile.Write(path, data, 0o644)
}

func (s *Store) Load(_ context.Context, namespace, key string) ([]byte, error) {
	path, err := s.resolvePath(namespace, key)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

func (s *Store) Delete(_ context.Context, namespace, key string) error {
	path, err := s.resolvePath(namespace, key)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) ListKeys(_ context.Context, namespace string) ([]string, error) {
	ns := security.SanitizeFilename(namespace)
	dir, err := security.SafePath(s.baseDir, ns)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		keys = append(keys, strings.TrimSuffix(entry.Name(), ".json"))
	}
	return keys, nil
}

func (s *Store) Close() error { return nil }
