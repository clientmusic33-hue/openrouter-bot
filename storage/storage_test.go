package storage_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"openrouter-bot/storage"
	jsonstore "openrouter-bot/storage/json"
	pgstore "openrouter-bot/storage/postgres"
)

func TestJSONStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	var s storage.Store = jsonstore.New(t.TempDir())
	defer s.Close()

	if s.Type() != "json" {
		t.Fatalf("Type() = %q, want json", s.Type())
	}

	payload := []byte(`{"hello":"world"}`)
	if err := s.Save(ctx, "notes", "user-42", payload); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := s.Load(ctx, "notes", "user-42")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(loaded) != string(payload) {
		t.Fatalf("Load() = %q, want %q", string(loaded), string(payload))
	}

	keys, err := s.ListKeys(ctx, "notes")
	if err != nil || len(keys) != 1 || keys[0] != "user-42" {
		t.Fatalf("ListKeys() = %v, %v; want [user-42]", keys, err)
	}

	if err := s.Delete(ctx, "notes", "user-42"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	after, err := s.Load(ctx, "notes", "user-42")
	if err != nil || after != nil {
		t.Fatalf("Load after Delete = %v, %v; want nil, nil", after, err)
	}
}

func TestPostgresStoreRejectsEmptyDSN(t *testing.T) {
	if _, err := pgstore.New("postgres", os.Getenv("UNSET_PG_DSN")); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty DSN error, got %v", err)
	}
}
