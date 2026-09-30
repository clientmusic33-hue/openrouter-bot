// Package provider manages a chain of AI backends.
//
// Each provider is an OpenAI-compatible endpoint with one or more models.
// Requests walk the chain in order: if a model fails, the next model of the
// same provider is tried; if the provider fails, the next provider is tried.
// This is what keeps the bot answering when one backend is rate limited,
// out of credit or simply down.
package provider

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sashabaranov/go-openai"
)

// cooldownAfter is how many consecutive failures put a provider temporarily
// at the back of the chain.
const cooldownAfter = 3

// cooldownDuration is how long a cooled-down provider stays at the back.
const cooldownDuration = 60 * time.Second

// Config describes one provider as written in config.yaml.
//
// APIKeyEnv names the environment variable holding the key; APIKey is the
// resolved value and is never written in the config file itself.
type Config struct {
	Name      string   `mapstructure:"name"`
	BaseURL   string   `mapstructure:"base_url"`
	APIKeyEnv string   `mapstructure:"api_key_env"`
	Models    []string `mapstructure:"models"`

	APIKey string `mapstructure:"-"`
}

// Provider is a configured backend with a ready to use client.
type Provider struct {
	config Config
	client *openai.Client
}

// NewProvider builds a client for the endpoint.
func NewProvider(cfg Config) (*Provider, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return nil, fmt.Errorf("provider requires a name")
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("provider %q requires base_url", name)
	}
	if len(cfg.Models) == 0 {
		return nil, fmt.Errorf("provider %q requires at least one model", name)
	}

	clientCfg := openai.DefaultConfig(cfg.APIKey)
	clientCfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	return &Provider{
		config: cfg,
		client: openai.NewClientWithConfig(clientCfg),
	}, nil
}

func (p *Provider) Name() string           { return p.config.Name }
func (p *Provider) BaseURL() string        { return p.config.BaseURL }
func (p *Provider) Models() []string       { return p.config.Models }
func (p *Provider) Client() *openai.Client { return p.client }

// HasModel reports whether the provider claims a model.
func (p *Provider) HasModel(model string) bool {
	for _, m := range p.config.Models {
		if strings.EqualFold(m, model) {
			return true
		}
	}

	return false
}

// -----------------------------------------------------------------------------
// CANDIDATES
// -----------------------------------------------------------------------------

// Candidate is one concrete (provider, model) pair to try.
type Candidate struct {
	Provider string
	Model    string

	client *openai.Client
}

// Stream opens a streaming completion against this candidate.
func (c Candidate) Stream(
	ctx context.Context,
	req openai.ChatCompletionRequest,
) (*openai.ChatCompletionStream, error) {
	req.Model = c.Model

	return c.client.CreateChatCompletionStream(ctx, req)
}

// -----------------------------------------------------------------------------
// HEALTH
// -----------------------------------------------------------------------------

type health struct {
	failures      int
	successes     int
	lastErr       string
	lastFailure   time.Time
	cooldownUntil time.Time
}

// -----------------------------------------------------------------------------
// CHAIN
// -----------------------------------------------------------------------------

// Chain is the ordered set of providers plus the currently selected model.
// All exported methods are safe for concurrent use.
type Chain struct {
	mu        sync.RWMutex
	providers []*Provider
	health    map[string]*health

	active int    // index of the preferred provider
	model  string // preferred model, may belong to any provider
}

// NewChain builds the chain from configuration. Providers whose key is
// missing are still created (they will simply fail at request time) so that
// /providers can report why.
func NewChain(cfgs []Config) (*Chain, error) {
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("no providers configured")
	}

	chain := &Chain{
		health: make(map[string]*health),
	}

	for _, cfg := range cfgs {
		provider, err := NewProvider(cfg)
		if err != nil {
			return nil, err
		}

		chain.providers = append(chain.providers, provider)
		chain.health[provider.Name()] = &health{}
	}

	if chain.providers[0].HasModel("") {
		return nil, fmt.Errorf("invalid provider configuration")
	}

	chain.model = chain.providers[0].Models()[0]

	return chain, nil
}

// Names returns the provider names in configured order.
func (c *Chain) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	names := make([]string, 0, len(c.providers))
	for _, p := range c.providers {
		names = append(names, p.Name())
	}

	return names
}

// ActiveName returns the currently preferred provider.
func (c *Chain) ActiveName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.providers[c.active].Name()
}

// ActiveModels returns the model list of the currently preferred provider.
func (c *Chain) ActiveModels() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.providers[c.active].Models()
}

// Model returns the currently selected model.
func (c *Chain) Model() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.model
}

// ActiveClient returns the client of the currently preferred provider.
// Used by features that are not part of the failover chain, such as
// translation.
func (c *Chain) ActiveClient() *openai.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.providers[c.active].Client()
}

// ActiveBaseURL returns the endpoint of the currently preferred provider.
func (c *Chain) ActiveBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.providers[c.active].BaseURL()
}

// ActiveAPIKey returns the key of the currently preferred provider.
func (c *Chain) ActiveAPIKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.providers[c.active].config.APIKey
}

// ModelsOf returns the models configured for a provider.
func (c *Chain) ModelsOf(name string) ([]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, p := range c.providers {
		if strings.EqualFold(p.Name(), name) {
			return p.Models(), true
		}
	}

	return nil, false
}

// SetActive switches the preferred provider.
func (c *Chain) SetActive(name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, p := range c.providers {
		if strings.EqualFold(p.Name(), name) {
			c.active = i
			// Move the model selection onto the new provider.
			c.model = p.Models()[0]

			return p.Name(), nil
		}
	}

	return "", fmt.Errorf("unknown provider %q", name)
}

// SetModel pins a model. It may belong to any provider; that provider becomes
// preferred so the first candidate matches the choice.
func (c *Chain) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("model must not be empty")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.model = model

	for i, p := range c.providers {
		if p.HasModel(model) {
			c.active = i
			break
		}
	}

	return nil
}

// ResetModel restores the default model of the active provider.
func (c *Chain) ResetModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.model = c.providers[c.active].Models()[0]

	return c.model
}

// -----------------------------------------------------------------------------
// CANDIDATE ORDERING
// -----------------------------------------------------------------------------

// Candidates returns every (provider, model) pair to try, best first.
//
// Ordering rules:
//  1. the pinned model on its own provider
//  2. the remaining models of the active provider
//  3. the other providers in rotation, each with all of its models
//  4. providers in cooldown go last, so a healthy chain recovers on its own
func (c *Chain) Candidates() []Candidate {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	count := len(c.providers)

	// Rotation starting at the active provider.
	order := make([]*Provider, 0, count)
	for i := 0; i < count; i++ {
		order = append(order, c.providers[(c.active+i)%count])
	}

	// Healthy first, cooling down last (stable within each group).
	sort.SliceStable(order, func(i, j int) bool {
		return !c.inCooldown(order[i].Name(), now) && c.inCooldown(order[j].Name(), now)
	})

	candidates := make([]Candidate, 0, count*4)
	pinned := false

	add := func(p *Provider, models []string) {
		for _, model := range models {
			if model == "" {
				continue
			}
			candidates = append(candidates, Candidate{
				Provider: p.Name(),
				Model:    model,
				client:   p.Client(),
			})
		}
	}

	// 1. pinned model first - but only while its provider is healthy.
	// Leading with a provider that is in cooldown defeats the point of the
	// health tracking.
	if c.model != "" {
		for _, p := range order {
			if p.HasModel(c.model) {
				if c.inCooldown(p.Name(), now) {
					break
				}
				add(p, []string{c.model})
				pinned = true
				break
			}
		}
	}

	// 2. + 3. everything else.
	seen := map[string]bool{}
	if pinned {
		seen[key(ownerOf(c.model, order), c.model)] = true
	}

	for _, p := range order {
		for _, model := range p.Models() {
			if seen[key(p.Name(), model)] {
				continue
			}
			seen[key(p.Name(), model)] = true
			add(p, []string{model})
		}
	}

	return candidates
}

func key(provider, model string) string {
	return provider + "\x00" + model
}

// ownerOf returns the name of the first provider claiming a model.
func ownerOf(model string, providers []*Provider) string {
	for _, p := range providers {
		if p.HasModel(model) {
			return p.Name()
		}
	}

	return ""
}

func (c *Chain) inCooldown(name string, now time.Time) bool {
	h, ok := c.health[name]
	if !ok {
		return false
	}

	return now.Before(h.cooldownUntil)
}

// -----------------------------------------------------------------------------
// HEALTH TRACKING
// -----------------------------------------------------------------------------

// RecordFailure notes that a provider failed. After cooldownAfter consecutive
// failures the provider drops to the back of the chain for a while.
func (c *Chain) RecordFailure(name string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	h := c.health[name]
	if h == nil {
		return
	}

	h.failures++
	h.lastErr = err.Error()
	h.lastFailure = time.Now()

	if h.failures >= cooldownAfter {
		h.cooldownUntil = time.Now().Add(cooldownDuration)
		log.Printf("Provider %s entering cooldown after %d failures: %v", name, h.failures, err)
	}
}

// RecordSuccess notes that a provider answered, clearing its failure streak.
func (c *Chain) RecordSuccess(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	h := c.health[name]
	if h == nil {
		return
	}

	h.successes++
	h.failures = 0
	h.lastErr = ""
	h.cooldownUntil = time.Time{}
}

// Info is the per-provider status shown by /providers.
type Info struct {
	Name       string
	BaseURL    string
	Models     []string
	Active     bool
	Current    bool // currently selected model belongs to this provider
	Failures   int
	Successes  int
	LastErr    string
	Cooldown   time.Duration
	Configured bool
}

// Status returns a snapshot for display.
func (c *Chain) Status() []Info {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	infos := make([]Info, 0, len(c.providers))

	for i, p := range c.providers {
		info := Info{
			Name:       p.Name(),
			BaseURL:    p.BaseURL(),
			Models:     p.Models(),
			Active:     i == c.active,
			Current:    p.HasModel(c.model),
			Configured: strings.TrimSpace(p.config.APIKey) != "",
		}

		if h := c.health[p.Name()]; h != nil {
			info.Failures = h.failures
			info.Successes = h.successes
			info.LastErr = h.lastErr
			if now.Before(h.cooldownUntil) {
				info.Cooldown = h.cooldownUntil.Sub(now).Round(time.Second)
			}
		}

		infos = append(infos, info)
	}

	return infos
}

// ProviderOf returns the provider that owns a model, if any.
func (c *Chain) ProviderOf(model string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, p := range c.providers {
		if p.HasModel(model) {
			return p.Name(), true
		}
	}

	return "", false
}
