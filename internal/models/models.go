// Package models discovers what a provider actually serves right now.
//
// Every backend the bot supports speaks the OpenAI-compatible model list
// endpoint, so one small adapter covers them all: the payload is normalised
// into Model and cached with a short TTL, because a catalogue changes far
// slower than a user clicks through the picker.
//
// The API key only ever travels in the Authorization header of the request
// this package builds. It is never logged, never cached and never part of an
// error.
package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"openrouter-bot/provider"
)

// Model is the normalised form of one entry of a provider's model list. Only
// what the provider actually returned is kept: nothing is invented.
type Model struct {
	// Provider is the bot side name of the backend that listed the model.
	Provider string `json:"provider"`
	// ModelID is the id to send in a completion request.
	ModelID string `json:"model_id"`
	// DisplayName is the human label, when the provider supplies one.
	DisplayName string `json:"display_name,omitempty"`
	// ContextLength is the context window in tokens, when reported.
	ContextLength int `json:"context_length,omitempty"`
}

const (
	// DefaultTTL is how long a discovered list is considered fresh.
	DefaultTTL = 5 * time.Minute
	// fetchTimeout bounds one discovery request.
	fetchTimeout = 12 * time.Second
	// maxModels caps a list so one huge catalogue cannot flood the picker.
	maxModels = 300
)

// listResponse is the OpenAI-compatible model list envelope. Providers that
// add fields simply leave them unread here.
type listResponse struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int    `json:"context_length"`
	} `json:"data"`
}

// Catalog caches discovered model lists. It is safe for concurrent use.
type Catalog struct {
	ttl time.Duration

	mu       sync.Mutex
	entries  map[string]entry
	inflight map[string]struct{}
}

type entry struct {
	models    []Model
	expiresAt time.Time
}

// New returns an empty catalogue with the default TTL.
func New() *Catalog {
	return &Catalog{
		ttl:       DefaultTTL,
		entries:   make(map[string]entry),
		inflight:  make(map[string]struct{}),
	}
}

// CacheKey builds the catalogue key for a provider. Built-in providers key on
// their name; a user owned provider passes its record id as scope, which is
// unique per user, so one user's list is never served to another.
func CacheKey(providerName, scope string) string {
	name := strings.ToLower(strings.TrimSpace(providerName))

	if owner := strings.TrimSpace(scope); owner != "" {
		sum := sha256.Sum256([]byte(owner))
		return "user:" + hex.EncodeToString(sum[:8])
	}

	return "provider:" + name
}

// Cached returns the models held for key while they are still fresh.
func (c *Catalog) Cached(key string) ([]Model, bool) {
	if c == nil || key == "" {
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	item, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	if time.Now().Before(item.expiresAt) {
		return item.models, true
	}

	delete(c.entries, key)

	return nil, false
}

// Invalidate drops a cached list so the next lookup fetches again. This is
// what the "refresh models" button uses.
func (c *Catalog) Invalidate(key string) {
	if c == nil || key == "" {
		return
	}

	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// Discover returns a provider's live model list, fetching it at most once per
// key while a request for the same key is already running. A failure is
// reported without any part of the credential in it, so the caller can fall
// back to a maintained list.
func (c *Catalog) Discover(ctx context.Context, key, baseURL, apiKey string) ([]Model, error) {
	if c == nil {
		return nil, fmt.Errorf("model catalogue is not configured")
	}
	if key == "" || strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("provider has no base url")
	}

	if list, ok := c.Cached(key); ok {
		return list, nil
	}

	c.mu.Lock()
	if _, running := c.inflight[key]; running {
		c.mu.Unlock()

		return c.awaitInflight(ctx, key)
	}
	c.inflight[key] = struct{}{}
	c.mu.Unlock()

	list, err := c.fetchAndStore(ctx, key, baseURL, apiKey)

	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()

	return list, err
}

// RefreshAsync refreshes a stale entry in the background and reports the
// result through done. A fresh entry, or a fetch that is already running, is
// left alone.
//
// apiKey is a function so that a user's credential is only materialised for
// the request itself and never parked in a closure that outlives it.
func (c *Catalog) RefreshAsync(key, baseURL string, apiKey func() string, done func([]Model, error)) {
	if c == nil || key == "" || strings.TrimSpace(baseURL) == "" {
		return
	}
	if _, fresh := c.Cached(key); fresh {
		return
	}

	c.mu.Lock()
	if _, running := c.inflight[key]; running {
		c.mu.Unlock()

		return
	}
	c.inflight[key] = struct{}{}
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.inflight, key)
			c.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()

		list, err := c.fetchAndStore(ctx, key, baseURL, apiKey())
		if done != nil {
			done(list, err)
		}
	}()
}

// awaitInflight waits for a discovery that another caller started, so two
// users opening the same provider cost one upstream request.
func (c *Catalog) awaitInflight(ctx context.Context, key string) ([]Model, error) {
	deadline := time.Now().Add(fetchTimeout + time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}

		if list, ok := c.Cached(key); ok {
			return list, nil
		}

		c.mu.Lock()
		_, running := c.inflight[key]
		c.mu.Unlock()

		if !running {
			break
		}
	}

	if list, ok := c.Cached(key); ok {
		return list, nil
	}

	return nil, fmt.Errorf("model discovery did not finish")
}

func (c *Catalog) fetchAndStore(ctx context.Context, key, baseURL, apiKey string) ([]Model, error) {
	list, err := fetchModels(ctx, baseURL, apiKey)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.entries[key] = entry{models: list, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()

	return list, nil
}

// fetchModels calls the provider's official OpenAI-compatible model list
// endpoint. The endpoint is the one the provider documents for model listing;
// the key is sent as a bearer token and appears nowhere else.
func fetchModels(ctx context.Context, baseURL, apiKey string) ([]Model, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("building models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	if key := strings.TrimSpace(apiKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := provider.SharedHTTPClient(fetchTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("model list request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The status line names the endpoint, never the credential.
		return nil, fmt.Errorf("model list returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("reading model list: %w", err)
	}

	var parsed listResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parsing model list: %w", err)
	}

	return normalise(parsed), nil
}

// normalise turns a provider specific payload into the common form, dropping
// duplicates and capping the size so the picker stays usable.
func normalise(parsed listResponse) []Model {
	out := make([]Model, 0, len(parsed.Data))
	seen := make(map[string]struct{}, len(parsed.Data))

	for _, item := range parsed.Data {
		id := strings.TrimSpace(item.ID)
		// The Gemini compatibility layer prefixes ids with "models/".
		id = strings.TrimPrefix(id, "models/")
		if id == "" {
			continue
		}

		key := strings.ToLower(id)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}

		model := Model{ModelID: id}
		if name := strings.TrimSpace(item.Name); name != "" && name != id {
			model.DisplayName = name
		}
		if item.ContextLength > 0 {
			model.ContextLength = item.ContextLength
		}

		out = append(out, model)

		if len(out) >= maxModels {
			break
		}
	}

	return out
}
