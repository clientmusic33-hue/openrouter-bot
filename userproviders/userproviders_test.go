package userproviders

import (
	"context"
	"strings"
	"testing"

	jsonstore "openrouter-bot/storage/json"
)

const testMasterKey = "unit-test-master-key"

func newTestStore(t *testing.T) *Store {
	t.Helper()

	return NewStore(jsonstore.New(t.TempDir()), testMasterKey)
}

func TestAddSealsTheKeyAndListsIt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	record, err := store.Add(ctx, 111, "Mistral", "mistral-small-latest", "https://api.mistral.ai/v1", "super-secret-key")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if strings.Contains(record.EncryptedAPIKey, "super-secret-key") {
		t.Fatal("the stored key must be sealed, not plaintext")
	}

	opened, err := record.APIKey(testMasterKey)
	if err != nil {
		t.Fatalf("APIKey: %v", err)
	}
	if opened != "super-secret-key" {
		t.Fatalf("APIKey = %q, want super-secret-key", opened)
	}

	list, err := store.List(ctx, 111)
	if err != nil || len(list) != 1 || list[0].Name != "Mistral" {
		t.Fatalf("List = %v, %v; want one Mistral entry", list, err)
	}
}

func TestOwnershipIsolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.Add(ctx, 111, "Mistral", "mistral-small-latest", "https://api.mistral.ai/v1", "key-a"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := store.Add(ctx, 222, "Groq", "llama-3.3-70b-versatile", "https://api.groq.com/openai/v1", "key-b"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	first, err := store.List(ctx, 111)
	if err != nil || len(first) != 1 || first[0].Name != "Mistral" {
		t.Fatalf("user 111 sees %v, %v", first, err)
	}

	second, err := store.List(ctx, 222)
	if err != nil || len(second) != 1 || second[0].Name != "Groq" {
		t.Fatalf("user 222 sees %v, %v", second, err)
	}

	// User 222 must not read, use or delete the provider of user 111.
	if _, ok, err := store.Get(ctx, 222, first[0].ID); err != nil || ok {
		t.Fatalf("Get across users = %v, %v; want false, nil", ok, err)
	}
	if _, ok, err := store.ProviderConfig(ctx, 222, "Mistral"); err != nil || ok {
		t.Fatalf("ProviderConfig across users = %v, %v; want false, nil", ok, err)
	}
	if removed, err := store.Remove(ctx, 222, first[0].ID); err != nil || removed {
		t.Fatalf("Remove across users = %v, %v; want false, nil", removed, err)
	}
	if _, err := store.APIKey(ctx, 222, first[0].ID); err == nil {
		t.Fatal("another user must not be able to open the key")
	}

	// The record of user 111 must still be there.
	if list, err := store.List(ctx, 111); err != nil || len(list) != 1 {
		t.Fatalf("List after the cross user attempts = %v, %v", list, err)
	}
}

func TestDisabledWithoutMasterKey(t *testing.T) {
	store := NewStore(jsonstore.New(t.TempDir()), "")

	if store.Enabled() {
		t.Fatal("a store without a master key must be disabled")
	}
	if _, err := store.Add(context.Background(), 111, "Mistral", "m", "https://api.mistral.ai/v1", "key"); err == nil {
		t.Fatal("Add must be refused without a master key")
	}
}

func TestSetModelAndRemove(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	record, err := store.Add(ctx, 111, "Mistral", "", "https://api.mistral.ai/v1", "key")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := store.SetModel(ctx, 111, record.ID, "open-mistral-nemo"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	cfg, ok, err := store.ProviderConfig(ctx, 111, "Mistral")
	if err != nil || !ok {
		t.Fatalf("ProviderConfig = %v, %v", ok, err)
	}
	if cfg.BaseURL != "https://api.mistral.ai/v1" || cfg.APIKey != "key" {
		t.Fatalf("unexpected endpoint or key: %#v", cfg)
	}
	if len(cfg.Models) != 1 || cfg.Models[0] != "open-mistral-nemo" {
		t.Fatalf("unexpected models: %#v", cfg.Models)
	}

	removed, err := store.Remove(ctx, 111, record.ID)
	if err != nil || !removed {
		t.Fatalf("Remove = %v, %v", removed, err)
	}

	list, err := store.List(ctx, 111)
	if err != nil || len(list) != 0 {
		t.Fatalf("List after Remove = %v, %v", list, err)
	}
}
