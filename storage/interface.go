// Package storage defines the persistence abstraction used for user state,
// long-term memories, reminders, notes, tasks, and group settings.
package storage

import "context"

// Store is the persistence interface implemented by the JSON file store and
// the optional PostgreSQL store.
type Store interface {
	// Type returns the backend identifier ("json" or "postgres").
	Type() string
	// Save stores a JSON-encoded document under namespace and key.
	Save(ctx context.Context, namespace, key string, data []byte) error
	// Load retrieves a JSON-encoded document from namespace and key.
	// Returns (nil, nil) when the document does not exist.
	Load(ctx context.Context, namespace, key string) ([]byte, error)
	// Delete removes a document from namespace and key.
	Delete(ctx context.Context, namespace, key string) error
	// ListKeys returns all keys stored under a namespace.
	ListKeys(ctx context.Context, namespace string) ([]string, error)
	// Close releases any underlying resources.
	Close() error
}
