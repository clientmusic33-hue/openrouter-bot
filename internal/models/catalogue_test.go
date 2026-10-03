package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const cataloguePayload = `{"data":[
	{"id":"qwen/qwen3-coder:free","name":"Qwen3 Coder","context_length":262144,
	 "pricing":{"prompt":"0","completion":"0"},
	 "architecture":{"modality":"text+image->text","input_modalities":["text","image"]},
	 "supported_parameters":["tools","reasoning"]},
	{"id":"meta-llama/llama-3.3-70b-instruct","context_length":131072,
	 "pricing":{"prompt":"0","completion":"0.0000006"}},
	{"id":"mystery/model","context_length":8192}
]}`

func newCatalogueServer(t *testing.T, payload string, calls *int) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
}

// TestFreeDetectionUsesPricingMetadata pins the rule: free means the provider
// reported a prompt price of zero and a completion price of zero. Missing
// pricing is unknown, never free.
func TestFreeDetectionUsesPricingMetadata(t *testing.T) {
	server := newCatalogueServer(t, cataloguePayload, nil)
	defer server.Close()

	list, err := New().Discover(context.Background(), "provider:test", server.URL, "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("Discover returned %d models, want 3", len(list))
	}

	if !list[0].Free || !list[0].PriceKnown {
		t.Errorf("zero prompt and completion must be free: %+v", list[0])
	}
	if list[1].Free || !list[1].PriceKnown {
		t.Errorf("a paid completion must not be free: %+v", list[1])
	}
	if list[2].Free || list[2].PriceKnown {
		t.Errorf("missing pricing must stay unknown: %+v", list[2])
	}
}

// TestCapabilityMetadataComesFromTheProvider keeps the picker from inventing
// capabilities: only what the model list declared is kept.
func TestCapabilityMetadataComesFromTheProvider(t *testing.T) {
	server := newCatalogueServer(t, cataloguePayload, nil)
	defer server.Close()

	list, err := New().Discover(context.Background(), "provider:test", server.URL, "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if !list[0].Vision || !list[0].Reasoning || !list[0].Tools {
		t.Errorf("declared capabilities were dropped: %+v", list[0])
	}
	if list[1].Vision || list[1].Reasoning || list[1].Tools {
		t.Errorf("capabilities were invented for a model that declared none: %+v", list[1])
	}
}

// TestConcurrentDiscoverSharesOneRequest covers two users opening the same
// provider at the same time.
func TestConcurrentDiscoverSharesOneRequest(t *testing.T) {
	var mu sync.Mutex
	calls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()

		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte(`{"data":[{"id":"one"},{"id":"two"}]}`))
	}))
	defer server.Close()

	catalog := New()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			list, err := catalog.Discover(context.Background(), "provider:test", server.URL, "")
			if err != nil || len(list) != 2 {
				t.Errorf("Discover returned %d models, %v", len(list), err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	if calls != 1 {
		t.Fatalf("the provider was called %d times, want 1", calls)
	}
}

// TestRefreshNowKeepsLastKnownGood is the forced refresh path behind
// /refresh_models: a provider that fails must not clear a working catalogue.
func TestRefreshNowKeepsLastKnownGood(t *testing.T) {
	failing := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing {
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}
		_, _ = w.Write([]byte(cataloguePayload))
	}))
	defer server.Close()

	catalog := New()
	key := "provider:openrouter"

	if _, err := catalog.Discover(context.Background(), key, server.URL, "k"); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	first, ok := catalog.FetchedAt(key)
	if !ok {
		t.Fatal("a successful fetch must be stamped")
	}

	failing = true

	done := make(chan struct{})
	if started := catalog.RefreshNow(key, server.URL, func() string { return "k" }, func(_ []Model, err error) {
		if err == nil {
			t.Error("the failing provider must be reported")
		}
		if strings.Contains(err.Error(), "k") {
			t.Errorf("the error leaks the key: %v", err)
		}
		close(done)
	}); !started {
		t.Fatal("a forced refresh must start even while the entry is fresh")
	}
	<-done

	if kept, fresh := catalog.Cached(key); !fresh || len(kept) != 3 {
		t.Errorf("a failed refresh must keep serving the last known catalogue: %d models", len(kept))
	}
	last, ok := catalog.LastKnown(key)
	if !ok || len(last) != 3 {
		t.Fatalf("the last known catalogue was lost: %d models", len(last))
	}
	if again, ok := catalog.FetchedAt(key); !ok || !again.Equal(first) {
		t.Error("a failed refresh must not move the last refresh timestamp")
	}
}

// TestStaleCatalogueSurvivesAForcedRefresh covers stale-while-refresh: the
// expired list is still readable while the upstream is unavailable.
func TestStaleCatalogueSurvivesAForcedRefresh(t *testing.T) {
	server := newCatalogueServer(t, cataloguePayload, nil)
	defer server.Close()

	catalog := New()
	catalog.ttl = time.Millisecond
	key := "provider:groq"

	if _, err := catalog.Discover(context.Background(), key, server.URL, ""); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	if _, fresh := catalog.Cached(key); fresh {
		t.Fatal("the entry should be stale by now")
	}
	stale, ok := catalog.LastKnown(key)
	if !ok || len(stale) == 0 {
		t.Fatal("the stale catalogue must stay readable as a fallback")
	}
}

// TestRefreshAsyncSkipsAFreshEntry keeps /models from hitting the providers on
// every call.
func TestRefreshAsyncSkipsAFreshEntry(t *testing.T) {
	calls := 0
	server := newCatalogueServer(t, `{"data":[{"id":"one"}]}`, &calls)
	defer server.Close()

	catalog := New()
	key := "provider:groq"

	if _, err := catalog.Discover(context.Background(), key, server.URL, ""); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	done := make(chan struct{}, 1)
	catalog.RefreshAsync(key, server.URL, func() string { return "" }, func([]Model, error) { done <- struct{}{} })

	select {
	case <-done:
		t.Fatal("a fresh entry must not be refreshed again")
	case <-time.After(50 * time.Millisecond):
	}
	if calls != 1 {
		t.Fatalf("the provider was called %d times, want 1", calls)
	}
}
