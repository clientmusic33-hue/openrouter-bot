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
	"net/http"
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

const (
	modelAvailableTTL = 2 * time.Minute
	modelLimitedTTL   = 45 * time.Second
	modelFailedTTL    = 90 * time.Second
)

// ModelAvailability is based only on real request outcomes. The bot never
// generates test prompts; a model without a recent successful/failed user
// request remains unknown.
type ModelAvailability string

const (
	ModelUnknown     ModelAvailability = "unknown"
	ModelAvailable   ModelAvailability = "available"
	ModelLimited     ModelAvailability = "limited"
	ModelUnavailable ModelAvailability = "unavailable"
)

type modelHealth struct {
	availability ModelAvailability
	expiresAt    time.Time
}

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

	// TimeoutSeconds is an optional per-provider timeout override.
	TimeoutSeconds int `mapstructure:"timeout_seconds"`

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
	// Use a shared pooled HTTP transport so TCP/TLS connections are reused
	// across completions and streams.
	clientCfg.HTTPClient = &http.Client{
		Transport: SharedTransport(),
	}

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
	Timeout  time.Duration

	client        *openai.Client
	healthTracked bool
}

// NewCandidate wraps a provider that was built outside the chain, which is how
// a backend a user added themselves joins the walk. The chain keeps ownership
// of health, ordering and failover; this only supplies the client to call.
func NewCandidate(p *Provider, model string) Candidate {
	var timeout time.Duration
	if cfg := p.Config(); cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	return Candidate{
		Provider: p.Name(),
		Model:    model,
		Timeout:  timeout,
		client:   p.Client(),
	}
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

	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

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
	timeouts      int
	cooldowns     int
	lastErr       string
	lastFailure   time.Time
	cooldownUntil time.Time
	latency       LatencyStats
}

// -----------------------------------------------------------------------------
// CHAIN
// -----------------------------------------------------------------------------

// Chain is the ordered set of providers plus the bot wide default model.
// All exported methods are safe for concurrent use.
type Chain struct {
	mu               sync.RWMutex
	providers        []*Provider
	health           map[string]*health
	modelLatency     map[string]*LatencyStats
	discoveredModels map[string][]string
	discoveredSets   map[string]map[string]struct{}
	modelHealth      map[string]modelHealth

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
		health:           make(map[string]*health),
		modelLatency:     make(map[string]*LatencyStats),
		discoveredModels: make(map[string][]string),
		discoveredSets:   make(map[string]map[string]struct{}),
		modelHealth:      make(map[string]modelHealth),
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

	return c.modelsForLocked(c.providers[c.active])
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

// EndpointOf returns the base URL and the key of a named provider. Model
// discovery needs the endpoint to ask for the catalogue; the key is handed to
// that request only.
func (c *Chain) EndpointOf(name string) (string, string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	p := c.providerNamed(name)
	if p == nil {
		return "", "", false
	}

	return p.BaseURL(), p.config.APIKey, true
}

// ModelsOf returns the models configured for a provider.
func (c *Chain) ModelsOf(name string) ([]string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, p := range c.providers {
		if strings.EqualFold(p.Name(), name) {
			return c.modelsForLocked(p), true
		}
	}

	return nil, false
}

// SetDiscoveredModels replaces the live catalogue for a provider. The full
// list is kept separate from configured fallback models, so menu discovery
// does not make every generation walk thousands of candidates.
func (c *Chain) SetDiscoveredModels(name string, modelIDs []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	p := c.providerNamed(name)
	if p == nil {
		return false
	}

	models := make([]string, 0, len(modelIDs))
	seen := make(map[string]struct{}, len(modelIDs))
	for _, id := range modelIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, id)
	}

	c.discoveredModels[p.Name()] = models
	c.discoveredSets[p.Name()] = seen
	return true
}

func (c *Chain) modelsForLocked(p *Provider) []string {
	models := append([]string(nil), p.Models()...)
	seen := make(map[string]struct{}, len(models)+len(c.discoveredModels[p.Name()]))
	for _, id := range models {
		seen[strings.ToLower(id)] = struct{}{}
	}
	for _, id := range c.discoveredModels[p.Name()] {
		key := strings.ToLower(id)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, id)
	}
	return models
}

func (c *Chain) hasDiscoveredModel(name, model string) bool {
	models := c.discoveredSets[name]
	_, ok := models[strings.ToLower(strings.TrimSpace(model))]
	return ok
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
		if p.HasModel(model) || c.hasDiscoveredModel(p.Name(), model) {
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

// CandidatesFor builds the ordered candidate list for one user. Known failed
// models and providers in an active cooldown are omitted instead of being
// retried at the end of the list. A dynamically discovered pinned model is
// inserted as the first candidate without expanding every discovered model
// into every request's fallback walk.
func (c *Chain) CandidatesFor(pref Preference, profile *UsageProfile) []Candidate {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := time.Now()
	count := len(c.providers)

	order := make([]*Provider, 0, count)
	for i := 0; i < count; i++ {
		order = append(order, c.providers[(c.active+i)%count])
	}

	if name := strings.TrimSpace(pref.Provider); name != "" {
		sort.SliceStable(order, func(i, j int) bool {
			return strings.EqualFold(order[i].Name(), name) && !strings.EqualFold(order[j].Name(), name)
		})
	}

	groups := make([][]Candidate, 0, count)
	for _, p := range order {
		if !p.Configured() || c.inCooldown(p.Name(), now) {
			// A keyless or open-circuit provider cannot answer right now. Do
			// not spend this request walking its models; it stays visible in
			// the provider picker and becomes eligible after cooldown.
			continue
		}

		group := make([]Candidate, 0, len(p.Models()))
		for _, model := range p.Models() {
			if strings.TrimSpace(model) == "" || c.modelCoolingDown(p.Name(), model, now) {
				continue
			}
			group = append(group, c.candidateLocked(p, model))
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

	pinned := strings.TrimSpace(pref.Model)
	if pinned == "" && !pref.Auto && strings.TrimSpace(pref.Provider) == "" {
		pinned = c.model
	}

	if pinned != "" && !c.modelCoolingDown(pref.Provider, pinned, now) {
		var owner *Provider
		if pref.Provider != "" {
			p := c.providerNamed(pref.Provider)
			ownsModel := p != nil && (p.HasModel(pinned) || c.hasDiscoveredModel(p.Name(), pinned))
			if ownsModel && p.Configured() && !c.inCooldown(p.Name(), now) {
				owner = p
			}
		}
		if owner == nil {
			owner = c.ownerOfUsable(pinned, now)
		}
		if owner != nil && !c.modelCoolingDown(owner.Name(), pinned, now) {
			found := false
			for _, candidate := range candidates {
				if strings.EqualFold(candidate.Provider, owner.Name()) && strings.EqualFold(candidate.Model, pinned) {
					found = true
					break
				}
			}
			if found {
				candidates = hoist(candidates, owner.Name(), pinned)
			} else {
				candidates = append([]Candidate{c.candidateLocked(owner, pinned)}, candidates...)
			}
		}
	}

	return candidates
}

// candidateLocked builds a candidate while the chain's read or write lock is held.
func (c *Chain) candidateLocked(p *Provider, model string) Candidate {
	var timeout time.Duration
	if p.config.TimeoutSeconds > 0 {
		timeout = time.Duration(p.config.TimeoutSeconds) * time.Second
	}

	return Candidate{
		Provider:      p.Name(),
		Model:         model,
		Timeout:       timeout,
		client:        p.Client(),
		healthTracked: true,
	}
}

// ownerOfUsable returns a provider that claims the model and can answer right
// now. Callers must hold at least a read lock.
func (c *Chain) ownerOfUsable(model string, now time.Time) *Provider {
	for _, p := range c.providers {
		if !p.HasModel(model) && !c.hasDiscoveredModel(p.Name(), model) {
			continue
		}
		if !p.Configured() || c.inCooldown(p.Name(), now) || c.modelCoolingDown(p.Name(), model, now) {
			continue
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
	if err == nil {
		return
	}

	class := ClassifyError(err)
	// A user pressing Stop cancels the context; that is not an upstream outage.
	if class == ErrClassCanceled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	h := c.health[name]
	if h == nil {
		return
	}

	h.failures++
	if class == ErrClassTimeout {
		h.timeouts++
	}
	h.lastErr = err.Error()
	h.lastFailure = time.Now()

	// Auth errors (401/403) cannot succeed on immediate retry, so open the
	// circuit breaker right away instead of burning 3 consecutive requests.
	if class == ErrClassAuth && h.failures < cooldownAfter {
		h.failures = cooldownAfter
	}

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

// RecordModelFailure caches a real generation failure for that exact model.
// Rate limits expire quickly; other failures are held briefly to prevent
// repeated attempts. Permanent model errors do not trip the whole provider's
// circuit breaker.
func (c *Chain) RecordModelFailure(name, model string, err error) {
	if err == nil {
		return
	}

	class := ClassifyError(err)
	if class == ErrClassCanceled {
		return
	}

	model = strings.TrimSpace(model)
	if model != "" {
		availability := ModelUnavailable
		ttl := modelFailedTTL
		if class == ErrClassRateLimit {
			availability = ModelLimited
			ttl = modelLimitedTTL
		}

		c.mu.Lock()
		if c.modelHealth == nil {
			c.modelHealth = make(map[string]modelHealth)
		}
		c.modelHealth[key(name, model)] = modelHealth{
			availability: availability,
			expiresAt:    time.Now().Add(ttl),
		}
		c.mu.Unlock()
	}

	if class != ErrClassPermanent {
		c.RecordFailure(name, err)
	}
}

// RecordSuccess notes that a provider answered, clearing its failure streak.
func (c *Chain) RecordSuccess(name string) {
	c.RecordSuccessLatency(name, "", 0)
}

// RecordModelSuccess records a successful real completion for a model and
// its short-lived availability observation. The status expires to unknown.
func (c *Chain) RecordModelSuccess(name, model string, latency time.Duration) {
	c.RecordSuccessLatency(name, model, latency)
	if model == "" {
		return
	}

	c.mu.Lock()
	if c.modelHealth == nil {
		c.modelHealth = make(map[string]modelHealth)
	}
	c.modelHealth[key(name, model)] = modelHealth{
		availability: ModelAvailable,
		expiresAt:    time.Now().Add(modelAvailableTTL),
	}
	c.mu.Unlock()
}

// ModelAvailability reports recent request health without probing the model.
// Missing credentials and an open provider circuit are conclusive; otherwise
// a model with no fresh real-request result is unknown.
func (c *Chain) ModelAvailability(name, model string) ModelAvailability {
	if c == nil {
		return ModelUnknown
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.modelAvailabilityLocked(name, model, time.Now())
}

func (c *Chain) modelAvailabilityLocked(name, model string, now time.Time) ModelAvailability {
	p := c.providerNamed(name)
	if p != nil {
		name = p.Name()
		if !p.Configured() {
			return ModelUnavailable
		}
	}

	if h := c.health[name]; h != nil && now.Before(h.cooldownUntil) {
		if ClassifyError(fmt.Errorf("%s", h.lastErr)) == ErrClassRateLimit {
			return ModelLimited
		}
		return ModelUnavailable
	}

	if observed, ok := c.modelHealth[key(name, model)]; ok && now.Before(observed.expiresAt) {
		return observed.availability
	}

	return ModelUnknown
}

// CanAttempt is checked immediately before a request as well as while
// building candidates, so concurrent requests stop reusing a model/provider
// that another request has just placed in cooldown.
func (c *Chain) CanAttempt(candidate Candidate) bool {
	if c == nil || !candidate.healthTracked {
		return true
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	p := c.providerNamed(candidate.Provider)
	if p == nil {
		return true
	}
	now := time.Now()
	return p.Configured() && !c.inCooldown(p.Name(), now) && !c.modelCoolingDown(p.Name(), candidate.Model, now)
}

func (c *Chain) modelCoolingDown(name, model string, now time.Time) bool {
	observed, ok := c.modelHealth[key(name, model)]
	return ok && now.Before(observed.expiresAt) &&
		(observed.availability == ModelLimited || observed.availability == ModelUnavailable)
}

// RecordSuccessLatency notes that a provider/model answered and records its
// response latency for adaptive routing.
func (c *Chain) RecordSuccessLatency(name, model string, latency time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	h := c.health[name]
	if h != nil {
		h.successes++
		h.failures = 0
		h.cooldowns = 0
		h.lastErr = ""
		h.cooldownUntil = time.Time{}
		if latency > 0 {
			h.latency.Record(latency)
		}
	}

	if latency > 0 && strings.TrimSpace(model) != "" {
		if c.modelLatency == nil {
			c.modelLatency = make(map[string]*LatencyStats)
		}
		key := strings.ToLower(strings.TrimSpace(model))
		stats := c.modelLatency[key]
		if stats == nil {
			stats = &LatencyStats{}
			c.modelLatency[key] = stats
		}
		stats.Record(latency)
	}
}

// ModelLatency returns the observed EWMA latency for a model, if any.
func (c *Chain) ModelLatency(model string) (time.Duration, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.modelLatency == nil {
		return 0, false
	}
	stats := c.modelLatency[strings.ToLower(strings.TrimSpace(model))]
	if stats == nil || stats.Samples == 0 {
		return 0, false
	}

	return stats.Avg, true
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
	Timeouts    int
	Cooldowns   int
	LastErr     string
	Cooldown    time.Duration
	AvgLatency  time.Duration
	LastLatency time.Duration
	Circuit     CircuitState
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
			Models:      c.modelsForLocked(p),
			Active:      i == c.active,
			Current:     p.HasModel(c.model) || c.hasDiscoveredModel(p.Name(), c.model),
			Configured:  p.Configured(),
			RequiresKey: p.RequiresKey(),
			Local:       p.Keyless(),
			Circuit:     CircuitClosed,
		}

		if h := c.health[p.Name()]; h != nil {
			info.Failures = h.failures
			info.Successes = h.successes
			info.Timeouts = h.timeouts
			info.Cooldowns = h.cooldowns
			info.LastErr = h.lastErr
			info.AvgLatency = h.latency.Avg.Round(time.Millisecond)
			info.LastLatency = h.latency.Last.Round(time.Millisecond)
			if now.Before(h.cooldownUntil) {
				info.Cooldown = h.cooldownUntil.Sub(now).Round(time.Second)
				info.Circuit = CircuitOpen
			} else if h.failures > 0 {
				info.Circuit = CircuitHalfOpen
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
		if p.HasModel(model) || c.hasDiscoveredModel(p.Name(), model) {
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
	Provider       string
	Model          string
	Description    string
	DisplayName    string
	SourceProvider string
	Availability   ModelAvailability
	Free           bool
	PriceKnown     bool
	// ContextLength is the context window in tokens when the provider
	// reported one during discovery. Zero means unknown.
	ContextLength int
	Current       bool
}

// ModelCatalog lists every (provider, model) pair, providers in chain order.
// Models whose provider has no key are still listed so the picker shows what
// a provider could offer.
func (c *Chain) ModelCatalog() []ModelRef {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var refs []ModelRef
	now := time.Now()
	for _, p := range c.providers {
		for _, model := range c.modelsForLocked(p) {
			refs = append(refs, ModelRef{
				Provider:     p.Name(),
				Model:        model,
				Description:  DescribeModel(model),
				Availability: c.modelAvailabilityLocked(p.Name(), model, now),
				Current:      strings.EqualFold(model, c.model),
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
