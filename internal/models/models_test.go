package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestDiscoverNormalisesAndCaches(t *testing.T) {
	calls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++

		if r.URL.Path != "/models" {
			w.WriteHeader(http.StatusNotFound)

			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"models/gemini-2.5-flash","name":"Gemini Flash","context_length":1048576,"pricing":{"prompt":"0","completion":"0"},"top_provider":{"name":"Google"}},
			{"id":"gpt-oss-120b","context_length":131072,"pricing":{"prompt":"0.0000002","completion":"0.0000006"}},
			{"id":"gpt-oss-120b"}
		]}`))
	}))
	defer server.Close()

	catalog := New()
	ctx := context.Background()

	list, err := catalog.Discover(ctx, "provider:test", server.URL, "test-key")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("Discover returned %d models, want 2: %#v", len(list), list)
	}
	if list[0].ModelID != "gemini-2.5-flash" {
		t.Errorf("the models/ prefix was not stripped: %q", list[0].ModelID)
	}
	if list[0].ContextLength != 1048576 {
		t.Errorf("context length = %d, want 1048576", list[0].ContextLength)
	}
	if list[0].DisplayName != "Gemini Flash" {
		t.Errorf("display name = %q, want Gemini Flash", list[0].DisplayName)
	}
	if list[0].SourceProvider != "Google" {
		t.Errorf("source provider = %q, want Google", list[0].SourceProvider)
	}
	if !list[0].PriceKnown || !list[0].Free {
		t.Errorf("free pricing was not retained: %+v", list[0])
	}
	if !list[1].PriceKnown || list[1].Free {
		t.Errorf("paid pricing was not retained: %+v", list[1])
	}

	again, err := catalog.Discover(ctx, "provider:test", server.URL, "test-key")
	if err != nil {
		t.Fatalf("cached Discover: %v", err)
	}
	if len(again) != 2 {
		t.Fatalf("cached Discover returned %d models, want 2", len(again))
	}
	if calls != 1 {
		t.Fatalf("the provider was called %d times, want 1", calls)
	}
}

func TestDiscoverReportsFailureWithoutTheKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	catalog := New()

	_, err := catalog.Discover(context.Background(), "provider:test", server.URL, "secret-key")
	if err == nil {
		t.Fatal("a failing model list must be reported")
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("the error leaks the key: %v", err)
	}
}

func TestInvalidateForcesARefetch(t *testing.T) {
	calls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":[{"id":"one"}]}`))
	}))
	defer server.Close()

	catalog := New()
	ctx := context.Background()

	if _, err := catalog.Discover(ctx, "provider:test", server.URL, ""); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	catalog.Invalidate("provider:test")

	if _, err := catalog.Discover(ctx, "provider:test", server.URL, ""); err != nil {
		t.Fatalf("Discover after Invalidate: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the provider was called %d times, want 2", calls)
	}
}

func TestCacheKeyIsolatesUserProviders(t *testing.T) {
	if CacheKey("groq", "") == CacheKey("groq", "record-a") {
		t.Fatal("a user scoped key must differ from the shared provider key")
	}
	if CacheKey("", "record-a") == CacheKey("", "record-b") {
		t.Fatal("two users must never share a catalogue key")
	}
	if CacheKey("groq", "") != CacheKey("Groq", "") {
		t.Fatal("provider keys must be case insensitive")
	}
}

func TestDiscoverKeepsCompleteCatalogue(t *testing.T) {
	var payload strings.Builder
	payload.WriteString(`{"data":[`)
	for i := 0; i < 350; i++ {
		if i > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString(`{"id":"vendor/model-`)
		payload.WriteString(strconv.Itoa(i))
		payload.WriteString(`"}`)
	}
	payload.WriteString(`]}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload.String()))
	}))
	defer server.Close()

	catalog := New()
	list, err := catalog.Discover(context.Background(), "provider:full", server.URL, "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(list) != 350 {
		t.Fatalf("Discover returned %d models, want the complete 350", len(list))
	}
}

func TestDiscoverBacksOffAfterRateLimit(t *testing.T) {
	calls := 0
	limited := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if limited {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"live-model"}]}`))
	}))
	defer server.Close()

	catalog := New()
	key := "provider:rate-limited"
	if _, err := catalog.Discover(context.Background(), key, server.URL, ""); err == nil {
		t.Fatal("expected the first rate-limited request to fail")
	}
	if _, err := catalog.Discover(context.Background(), key, server.URL, ""); err == nil {
		t.Fatal("expected a recent failure to suppress another request")
	}
	if calls != 1 {
		t.Fatalf("made %d requests during backoff, want 1", calls)
	}

	limited = false
	catalog.Invalidate(key)
	if _, err := catalog.Discover(context.Background(), key, server.URL, ""); err != nil {
		t.Fatalf("Discover after explicit invalidation: %v", err)
	}
	if calls != 2 {
		t.Fatalf("made %d requests after invalidation, want 2", calls)
	}
}
