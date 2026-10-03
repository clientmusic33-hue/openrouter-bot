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
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"openrouter-bot/provider"
)

// Model is the normalised form of one entry of a provider's model list. Only
// what the provider actually returned is kept: nothing is invented.
type Model struct {
	// Provider is the bot-side name of the backend that listed the model.
	Provider string `json:"provider"`
	// ModelID is the id to send in a completion request.
	ModelID string `json:"model_id"`
	// DisplayName is the human label, when the provider supplies one.
	DisplayName string `json:"display_name,omitempty"`
	// SourceProvider is the upstream publisher/provider, usually the
	// namespace before '/' in an OpenRouter model id.
	SourceProvider string `json:"source_provider,omitempty"`
	// ContextLength is the context window in tokens, when reported.
	ContextLength int `json:"context_length,omitempty"`
	// Pricing is copied from the model catalogue. Missing fields remain empty
	// so callers never mistake missing pricing for a free model.
	Pricing    Pricing `json:"pricing,omitempty"`
	PriceKnown bool    `json:"price_known,omitempty"`
	Free       bool    `json:"free,omitempty"`

	// Vision, Reasoning and Tools are set only when the provider reported
	// them, so a capability is never guessed.
	Vision    bool `json:"vision,omitempty"`
	Reasoning bool `json:"reasoning,omitempty"`
	Tools     bool `json:"tools,omitempty"`
}

// Pricing contains per-token prompt and completion prices in USD.
type Pricing struct {
	Prompt     string `json:"prompt,omitempty"`
	Completion string `json:"completion,omitempty"`
}

const (
	// DefaultTTL is how long a discovered list is considered fresh.
	DefaultTTL = 5 * time.Minute
	// fetchTimeout bounds one discovery request.
	fetchTimeout = 12 * time.Second
	// failedFetchTTL dampens repeated requests after an upstream error or
	// rate limit. An explicit refresh can bypass it.
	failedFetchTTL = 30 * time.Second
)

// listResponse is the OpenAI-compatible model list envelope. Providers that
// add fields simply leave them unread here. OpenRouter's model list also
// supplies pricing and publisher information, so those fields are retained.
type listResponse struct {
	Data []struct {
		ID            string          `json:"id"`
		Name          string          `json:"name"`
		ContextLength int             `json:"context_length"`
		Provider      json.RawMessage `json:"provider"`
		ProviderName  string          `json:"provider_name"`
		Pricing       struct {
			Prompt     json.RawMessage `json:"prompt"`
			Completion json.RawMessage `json:"completion"`
		} `json:"pricing"`
		TopProvider struct {
			Name string `json:"name"`
		} `json:"top_provider"`
		Architecture struct {
			Modality        string   `json:"modality"`
			InputModalities []string `json:"input_modalities"`
		} `json:"architecture"`
		SupportedParameters []string `json:"supported_parameters"`
	} `json:"data"`
}

// Catalog caches discovered model lists. It is safe for concurrent use.
type Catalog struct {
	ttl time.Duration

	mu          sync.Mutex
	entries     map[string]entry
	failedUntil map[string]time.Time
	inflight    map[string]struct{}
}

type entry struct {
	models    []Model
	expiresAt time.Time
	fetchedAt time.Time
}

// New returns an empty catalogue with the default TTL.
func New() *Catalog {
	return &Catalog{
		ttl:         DefaultTTL,
		entries:     make(map[string]entry),
		failedUntil: make(map[string]time.Time),
		inflight:    make(map[string]struct{}),
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
		return cloneModels(item.models), true
	}

	// Keep the last good response for stale-while-refresh fallback. Expired
	// metadata is not presented as fresh, but is safer than an empty picker
	// while the upstream is unavailable.
	return nil, false
}

// LastKnown returns the most recent successful catalogue even when its TTL
// has elapsed. It is used only as a display fallback while a refresh runs or
// after a temporary upstream error.
func (c *Catalog) LastKnown(key string) ([]Model, bool) {
	if c == nil || key == "" {
		return nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	item, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	return cloneModels(item.models), true
}

func cloneModels(source []Model) []Model {
	return append([]Model(nil), source...)
}

// Invalidate drops a cached list so the next lookup fetches again. This is
// what the "refresh models" button uses.
func (c *Catalog) Invalidate(key string) {
	if c == nil || key == "" {
		return
	}

	c.mu.Lock()
	delete(c.entries, key)
	delete(c.failedUntil, key)
	c.mu.Unlock()
}

// Discover returns a provider's live model list, fetching it at most once per
// key while a request for the same key is already running. A failure is
// reported without any part of the credential in it, so the caller can fall
// back to its last known catalogue or configured models.
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
	if c.failedRecentlyLocked(key, time.Now()) {
		c.mu.Unlock()
		return nil, fmt.Errorf("model discovery temporarily paused after a recent failure")
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
// result through done. A fresh entry, a recent failure, or a fetch that is
// already running is left alone.
//
// apiKey is a function so that a user's credential is only materialised for
// the request itself and never parked in a closure that outlives it.
func (c *Catalog) RefreshAsync(key, baseURL string, apiKey func() string, done func([]Model, error)) {
	c.refreshAsync(key, baseURL, apiKey, done, false)
}

// RefreshNowAsync bypasses freshness and failure backoff while keeping the
// last good catalogue available if the forced request fails.
func (c *Catalog) RefreshNowAsync(key, baseURL string, apiKey func() string, done func([]Model, error)) {
	c.refreshAsync(key, baseURL, apiKey, done, true)
}

// RefreshNow is RefreshNowAsync for callers that need to know whether the
// request was actually started, so they can wait for exactly as many
// callbacks as they will receive.
func (c *Catalog) RefreshNow(key, baseURL string, apiKey func() string, done func([]Model, error)) bool {
	return c.refreshAsync(key, baseURL, apiKey, done, true)
}

func (c *Catalog) refreshAsync(key, baseURL string, apiKey func() string, done func([]Model, error), force bool) bool {
	if c == nil || key == "" || strings.TrimSpace(baseURL) == "" || apiKey == nil {
		return false
	}
	if !force {
		if _, fresh := c.Cached(key); fresh {
			return false
		}
	}

	c.mu.Lock()
	if _, running := c.inflight[key]; running {
		c.mu.Unlock()

		return false
	}
	if !force && c.failedRecentlyLocked(key, time.Now()) {
		c.mu.Unlock()
		return false
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

	return true
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
	if err == nil && len(list) == 0 {
		err = fmt.Errorf("model list contained no models")
	}
	if err != nil {
		c.mu.Lock()
		c.failedUntil[key] = time.Now().Add(failedFetchTTL)
		c.mu.Unlock()
		return nil, err
	}

	if strings.HasPrefix(key, "provider:") {
		name := strings.TrimPrefix(key, "provider:")
		for i := range list {
			list[i].Provider = name
		}
	}

	now := time.Now()

	c.mu.Lock()
	c.entries[key] = entry{models: cloneModels(list), expiresAt: now.Add(c.ttl), fetchedAt: now}
	delete(c.failedUntil, key)
	c.mu.Unlock()

	return list, nil
}

// FetchedAt reports when a key was last filled from upstream. It keeps the
// timestamp of the last good response, so a failed refresh does not lose it.
func (c *Catalog) FetchedAt(key string) (time.Time, bool) {
	if c == nil || key == "" {
		return time.Time{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	item, ok := c.entries[key]
	if !ok {
		return time.Time{}, false
	}

	return item.fetchedAt, true
}

// Refreshing reports whether a discovery request for the key is in flight.
func (c *Catalog) Refreshing(key string) bool {
	if c == nil || key == "" {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	_, running := c.inflight[key]

	return running
}

func (c *Catalog) failedRecentlyLocked(key string, now time.Time) bool {
	until, ok := c.failedUntil[key]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}

	delete(c.failedUntil, key)
	return false
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("reading model list: %w", err)
	}

	var parsed listResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parsing model list: %w", err)
	}

	return normalise(parsed), nil
}

// normalise turns a provider-specific payload into the common form and drops
// duplicate ids. It deliberately does not cap the list: OpenRouter's endpoint
// is the source of truth for the complete model picker.
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

		model.SourceProvider = firstNonEmpty(
			providerName(item.Provider),
			item.ProviderName,
			item.TopProvider.Name,
		)
		if model.SourceProvider == "" {
			if namespace, _, found := strings.Cut(id, "/"); found {
				model.SourceProvider = namespace
			}
		}

		prompt, promptOK := parsePrice(item.Pricing.Prompt)
		completion, completionOK := parsePrice(item.Pricing.Completion)
		model.Pricing = Pricing{Prompt: prompt, Completion: completion}
		model.PriceKnown = promptOK && completionOK
		model.Free = model.PriceKnown && isZeroPrice(prompt) && isZeroPrice(completion)

		model.Vision = reportsVision(item.Architecture.Modality, item.Architecture.InputModalities)
		for _, param := range item.SupportedParameters {
			switch strings.ToLower(strings.TrimSpace(param)) {
			case "reasoning":
				model.Reasoning = true
			case "tools", "tool_choice":
				model.Tools = true
			}
		}

		out = append(out, model)
	}

	return out
}

// reportsVision reads the modality the provider declared for a model.
func reportsVision(modality string, inputs []string) bool {
	for _, input := range inputs {
		if strings.Contains(strings.ToLower(input), "image") {
			return true
		}
	}

	return strings.Contains(strings.ToLower(strings.TrimSpace(modality)), "image")
}

func providerName(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	var name string
	if json.Unmarshal(raw, &name) == nil {
		return strings.TrimSpace(name)
	}

	var details struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &details) == nil {
		return strings.TrimSpace(details.Name)
	}

	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}

	return ""
}

func parsePrice(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}

	var value string
	if json.Unmarshal(raw, &value) != nil {
		value = string(raw)
	}
	value = strings.TrimSpace(value)
	price, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(price) || math.IsInf(price, 0) || price < 0 {
		return "", false
	}

	return value, true
}

func isZeroPrice(raw string) bool {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return err == nil && value == 0
}
