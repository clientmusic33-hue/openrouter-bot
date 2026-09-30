// Package provider manages a chain of AI backends.
//
// Each provider is an OpenAI-compatible endpoint with one or more models.
// Requests walk the chain in order: if a model fails, the next model of the
// same provider is tried; if the provider fails, the next provider is tried.
// This is what keeps the bot answering when one backend is rate limited,
// out of credit or simply down.
//
// The chain is also the place where a user's own choice lives: a Preference
// pins a provider, a model, or neither (auto mode, where the routing score in
// recommend.go decides).
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

// cooldownDuration is the base time a cooled-down provider stays at the back.
// Repeated outages double it, up to cooldownMax.
const cooldownDuration = 60 * time.Second

// cooldownMax caps the growing backoff.
const cooldownMax = 10 * time.Minute

// Config describes one provider as written in config.yaml.
//
// APIKeyEnv names the environment variable holding the key; APIKey is the
// resolved value and is never written in the config file itself. Any field
// left empty is filled from the built-in preset of the same name, so
//
//	providers:
//	  - name: groq
//
// is a complete, working entry.
type Config struct {
	Name      string   `mapstructure:"name"`
	BaseURL   string   `mapstructure:"base_url"`
	APIKeyEnv string   `mapstructure:"api_key_env"`
	Models    []string `mapstructure:"models"`

	// RequiresKey is set for hosted backends. A provider that needs a key and
	// has none is kept in the list (so /providers can explain why it is
	// skipped) but never used for a request.
	RequiresKey bool `mapstructure:"requires_key"`

	// APIKey is the resolved secret. It is never serialised.
	APIKey string `mapstructure:"-"`
}

// Configured reports whether the provider can actually serve a request.
func (c Config) Configured() bool {
	if !c.RequiresKey {
		return true
	}

	return strings.TrimSpace(c.APIKey) != ""
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

	cfg = ApplyPreset(cfg)

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
func (p *Provider) Config() Config         { return p.config }
func (p *Provider) Configured() bool       { return p.config.Configured() }
func (p *Provider) RequiresKey() bool      { return p.config.RequiresKey }
func (p *Provider) Keyless() bool          { return !p.config.RequiresKey }
func (p *Provider) HasAPIKey() bool        { return strings.TrimSpace(p.config.APIKey) != "" }
func (p *Provider) Docs() string {
	preset, ok := LookupPreset(p.config.Name)
	if !ok {
		return ""
	}

	return preset.Docs
}

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

// Complete runs a non-streaming completion against this candidate. Features
// that must see the whole answer before showing anything (translations) use
// it, and it participates in the same health tracking as Stream.
func (c Candidate) Complete(
	ctx context.Context,
	req openai.ChatCompletionRequest,
) (openai.ChatCompletionResponse, error) {
	req.Model = c.Model

	return c.client.CreateChatCompletion(ctx, req)
}

// Preference is one user's routing choice.
//
// Zero value means auto: the bot scores every model against the user's usage
// and starts with the best one, falling back through the rest.
type Preference struct {
	// Provider pins a provider. Empty means any.
	Provider string
	// Model pins a model. Empty means any model of Provider, or any model at
	// all when Provider is empty too.
	Model string
	// Auto is true while the bot chooses the model for the user. Choosing a
	// model or a provider explicitly turns it off.
	Auto bool
}

// Describe renders the preference for the UI.
func (p Preference) Describe() string {
	switch {
	case !p.Auto && p.Model != "":
		return p.Model
	case p.Provider != "":
		return "auto on " + p.Provider
	default:
		return "auto (best model for your usage)"
	}
}

// -----------------------------------------------------------------------------
// HEALTH
// -----------------------------------------------------------------------------

type health struct {
	failures      int
	successes     int
	cooldowns     int
	lastErr       string
	lastFailure   time.Time
	cooldownUntil time.Time
}

// -----------------------------------------------------------------------------
// CHAIN
// -----------------------------------------------------------------------------

// Chain is the ordered set of providers plus the bot wide default model.
// All exported methods are safe for concurrent use.
type Chain struct {
	mu        sync.RWMutex
	providers []*Provider
	health    map[string]*health

	active int    // index of the preferred provider (admin default)
	model  string // admin default model, may belong to any provider
}

// NewChain builds the chain from configuration. Providers whose key is
// missing are still created (they are simply never tried) so that /providers
// can report why.
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

	chain.model = chain.providers[0].Models()[0]

	return chain, nil
}

// Names returns the provider names in configured order.
func (c *Chain) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.namesLocked()
}

func (c *Chain) namesLocked() []string {
	names := make([]string, 0, len(c.providers))
	for _, p := range c.providers {
		names = append(names, p.Name())
	}

	return names
}

// UsableNames returns the providers that can answer right now, in chain
// order. Providers without a key are left out.
func (c *Chain) UsableNames() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	names := make([]string, 0, len(c.providers))
	for _, p := range c.providers {
		if p.Configured() {
			names = append(names, p.Name())
		}
	}

	return names
}

// ActiveName returns the bot wide preferred provider.
func (c *Chain) ActiveName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.providers[c.active].Name()
}

// ActiveModels returns the model list of the bot wide preferred provider.
func (c *Chain) ActiveModels() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.providers[c.active].Models()
}

// Model returns the bot wide default model.
func (c *Chain) Model() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.model
}

// ActiveClient returns the client of the bot wide preferred provider.
// Used by features that are not part of the failover chain, such as
// translation.
func (c *Chain) ActiveClient() *openai.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.providers[c.active].Client()
}

// ActiveBaseURL returns the endpoint of the bot wide preferred provider.
func (c *Chain) ActiveBaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.providers[c.active].BaseURL()
}

// ActiveAPIKey returns the key of the bot wide preferred provider.
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

// SetActive switches the bot wide preferred provider.
func (c *Chain) SetActive(name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, p := range c.providers {
		if strings.EqualFold(p.Name(), name) {
			c.active = i
			// Move the default model selection onto the new provider.
			c.model = p.Models()[0]

			return p.Name(), nil
		}
	}

	return "", fmt.Errorf("unknown provider %q", name)
}

// SetModel pins the bot wide default model. It may belong to any provider;
// that provider becomes preferred so the first candidate matches the choice.
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

// CostEndpoint returns the endpoint and key needed to look up the cost of a
// generation from the provider that answered. The third value is false for
// providers that do not expose OpenRouter-style generation statistics (Groq,
// Gemini, Cerebras and friends report their usage in the response instead).
func (c *Chain) CostEndpoint(name string) (string, string, bool) {
	p := c.providerNamed(name)
	if p == nil || !p.Configured() || !p.HasAPIKey() {
		return "", "", false
	}

	if !strings.Contains(strings.ToLower(p.BaseURL()), "openrouter.ai") {
		return "", "", false
	}

	return p.BaseURL(), p.Config().APIKey, true
}

// Candidates returns every (provider, model) pair to try, best first, using
// the bot wide default preference.
func (c *Chain) Candidates() []Candidate {
	return c.CandidatesFor(Preference{}, nil)
}

// CandidatesFor builds the ordered candidate list for one user.
//
// Ordering rules:
//  1. the model the user pinned always leads, unless its provider is cooling
//     down - a degraded backend must not be kept alive by a pin
//  2. the user's pinned provider comes first, the rest follow in chain order
//  3. providers without a key and providers in cooldown sink to the bottom,
//     so an unusable backend costs no latency
//  4. with a usage profile (and no pin) every model is scored, which is what
//     makes automatic routing pick the best model for how the user works
func (c *Chain) CandidatesFor(pref Preference, profile *UsageProfile) []Candidate {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	count := len(c.providers)

	// Rotation starting at the bot wide active provider, then health sorted.
	order := make([]*Provider, 0, count)
	for i := 0; i < count; i++ {
		order = append(order, c.providers[(c.active+i)%count])
	}

	if name := strings.TrimSpace(pref.Provider); name != "" {
		sort.SliceStable(order, func(i, j int) bool {
			return strings.EqualFold(order[i].Name(), name) && !strings.EqualFold(order[j].Name(), name)
		})
	}

	sort.SliceStable(order, func(i, j int) bool {
		return rankProvider(order[i], c, now) < rankProvider(order[j], c, now)
	})

	// One model group per usable provider.
	groups := make([][]Candidate, 0, count)
	for _, p := range order {
		if !p.Configured() {
			// Without a key the request can only fail, so it is not worth a
			// round trip. The provider stays visible in /providers.
			continue
		}

		group := make([]Candidate, 0, len(p.Models()))
		for _, model := range p.Models() {
			if strings.TrimSpace(model) == "" {
				continue
			}
			group = append(group, Candidate{
				Provider: p.Name(),
				Model:    model,
				client:   p.Client(),
			})
		}
		groups = append(groups, group)
	}

	// Scoring. Automatic mode ranks every model together; a pinned provider
	// ranks inside each group so its models still come first.
	if profile != nil {
		if pref.Auto && strings.TrimSpace(pref.Provider) == "" {
			groups = [][]Candidate{rank(flatten(groups), *profile, c, now)}
		} else {
			for i := range groups {
				groups[i] = rank(groups[i], *profile, c, now)
			}
		}
	}

	candidates := flatten(groups)

	// The pinned model leads. Without an explicit choice the chain default
	// keeps leading, which is what a zero value Preference means.
	pinned := strings.TrimSpace(pref.Model)
	if pinned == "" && !pref.Auto && strings.TrimSpace(pref.Provider) == "" {
		pinned = c.model
	}

	if pinned != "" {
		if p := c.ownerOfUsable(pinned, now); p != nil {
			candidates = hoist(candidates, p.Name(), pinned)
		}
	}

	return candidates
}

// ownerOfUsable returns the provider owning a model when it can answer right
// now. Callers must hold at least a read lock.
func (c *Chain) ownerOfUsable(model string, now time.Time) *Provider {
	for _, p := range c.providers {
		if !p.HasModel(model) {
			continue
		}
		if !p.Configured() || c.inCooldown(p.Name(), now) {
			return nil
		}

		return p
	}

	return nil
}

// flatten concatenates model groups, dropping duplicates.
func flatten(groups [][]Candidate) []Candidate {
	total := 0
	for _, group := range groups {
		total += len(group)
	}

	seen := make(map[string]bool, total)
	out := make([]Candidate, 0, total)

	for _, group := range groups {
		for _, candidate := range group {
			key := key(candidate.Provider, candidate.Model)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, candidate)
		}
	}

	return out
}

// hoist moves one (provider, model) pair to the front of the list. The model
// is a single user's pinned choice, so "not found" simply leaves the list
// untouched.
func hoist(candidates []Candidate, provider, model string) []Candidate {
	for i, candidate := range candidates {
		if !strings.EqualFold(candidate.Provider, provider) || !strings.EqualFold(candidate.Model, model) {
			continue
		}

		out := make([]Candidate, 0, len(candidates))
		out = append(out, candidate)
		out = append(out, candidates[:i]...)
		out = append(out, candidates[i+1:]...)

		return out
	}

	return candidates
}

// providerNamed resolves a provider name back to its entry. Callers must hold
// at least a read lock.
func (c *Chain) providerNamed(name string) *Provider {
	for _, p := range c.providers {
		if strings.EqualFold(p.Name(), name) {
			return p
		}
	}

	return nil
}

// rankProvider orders providers for the candidate walk: usable and healthy
// first, keyless last, cooling down last of all.
func rankProvider(p *Provider, c *Chain, now time.Time) int {
	switch {
	case !p.Configured():
		return 3
	case c.inCooldown(p.Name(), now):
		return 2
	default:
		return 0
	}
}

func key(provider, model string) string {
	return provider + "\x00" + model
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
// failures the provider drops to the back of the chain, and each further
// outage widens the window (60s, 2m, 4m … up to 10m).
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
		h.cooldowns++
		cooldown := cooldownDuration << (h.cooldowns - 1)
		if cooldown > cooldownMax || cooldown <= 0 {
			cooldown = cooldownMax
		}
		h.cooldownUntil = time.Now().Add(cooldown)
		log.Printf(
			"Provider %s entering cooldown for %s after %d failures: %v",
			name, cooldown, h.failures, err,
		)
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
	h.cooldowns = 0
	h.lastErr = ""
	h.cooldownUntil = time.Time{}
}

// Info is the per-provider status shown by /providers.
type Info struct {
	Name        string
	BaseURL     string
	Models      []string
	Active      bool
	Current     bool // currently selected model belongs to this provider
	Failures    int
	Successes   int
	Cooldowns   int
	LastErr     string
	Cooldown    time.Duration
	Configured  bool
	RequiresKey bool
	Local       bool
}

// Healthy reports whether the provider is usable right now.
func (i Info) Healthy() bool {
	return i.Configured && i.Cooldown == 0
}

// Status returns a snapshot for display.
func (c *Chain) Status() []Info {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	infos := make([]Info, 0, len(c.providers))

	for i, p := range c.providers {
		info := Info{
			Name:        p.Name(),
			BaseURL:     p.BaseURL(),
			Models:      p.Models(),
			Active:      i == c.active,
			Current:     p.HasModel(c.model),
			Configured:  p.Configured(),
			RequiresKey: p.RequiresKey(),
			Local:       p.Keyless(),
		}

		if h := c.health[p.Name()]; h != nil {
			info.Failures = h.failures
			info.Successes = h.successes
			info.Cooldowns = h.cooldowns
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

// HasModel reports whether any configured provider claims a model.
func (c *Chain) HasModel(model string) bool {
	_, ok := c.ProviderOf(model)

	return ok
}

// ModelRef is one selectable model in the UI: its id plus the provider that
// owns it and a short capability description.
type ModelRef struct {
	Provider    string
	Model       string
	Description string
	Current     bool
}

// ModelCatalog lists every (provider, model) pair, providers in chain order.
// Models whose provider has no key are still listed so the picker shows what
// a provider could offer.
func (c *Chain) ModelCatalog() []ModelRef {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var refs []ModelRef
	for _, p := range c.providers {
		for _, model := range p.Models() {
			refs = append(refs, ModelRef{
				Provider:    p.Name(),
				Model:       model,
				Description: DescribeModel(model),
				Current:     strings.EqualFold(model, c.model),
			})
		}
	}

	return refs
}

// Count returns the number of configured providers.
func (c *Chain) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.providers)
}
