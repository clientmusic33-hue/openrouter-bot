package provider

import (
	"errors"
	"testing"
)

// errRateLimited stands in for the upstream 429 the health tracker reacts to.
var errRateLimited = errors.New("429 rate limited")

// routingChain builds a chain with two usable providers and one vision
// capable model, which is enough to exercise every ordering rule.
func routingChain(t *testing.T) *Chain {
	t.Helper()

	chain, err := NewChain([]Config{
		{
			Name:    "groq",
			BaseURL: "https://api.groq.com/openai/v1",
			APIKey:  "groq-key",
			Models:  []string{"llama-3.3-70b-versatile", "llama-3.1-8b-instant"},
		},
		{
			Name:    "gemini",
			BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/",
			APIKey:  "gemini-key",
			Models:  []string{"gemini-2.5-flash"},
		},
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	return chain
}

func TestCandidatesForPinnedModelLeads(t *testing.T) {
	chain := routingChain(t)

	candidates := chain.CandidatesFor(Preference{Model: "gemini-2.5-flash"}, nil)
	if len(candidates) == 0 {
		t.Fatal("no candidates")
	}
	if candidates[0].Model != "gemini-2.5-flash" || candidates[0].Provider != "gemini" {
		t.Fatalf("first candidate = %+v, want the pinned gemini model", candidates[0])
	}

	// Every configured model must still be reachable as a fallback.
	if len(candidates) != 3 {
		t.Fatalf("got %d candidates, want 3", len(candidates))
	}
}

func TestCandidatesForPinnedProviderLeads(t *testing.T) {
	chain := routingChain(t)

	candidates := chain.CandidatesFor(Preference{Provider: "gemini"}, nil)
	if len(candidates) == 0 {
		t.Fatal("no candidates")
	}
	if candidates[0].Provider != "gemini" {
		t.Fatalf("first candidate = %+v, want gemini", candidates[0])
	}
}

// TestCandidatesForAutoScoresModels is the automatic routing promise: with a
// usage profile the best model for that profile leads, whatever the chain
// order is.
func TestCandidatesForAutoScoresModels(t *testing.T) {
	chain := routingChain(t)

	// A user who sends photos needs a vision model, and only Gemini has it.
	profile := UsageProfile{UsesVision: true}
	candidates := chain.CandidatesFor(Preference{Auto: true}, &profile)

	if candidates[0].Model != "gemini-2.5-flash" {
		t.Fatalf("auto routing picked %+v, want the vision capable model", candidates[0])
	}
}

// TestAutoRoutingAvoidsDislikedModel keeps user feedback honest: a model the
// user downvoted must not lead the automatic routing any more.
func TestAutoRoutingAvoidsDislikedModel(t *testing.T) {
	chain := routingChain(t)

	// Without feedback the largest model wins for a light user.
	plain := UsageProfile{}
	if lead := chain.CandidatesFor(Preference{Auto: true}, &plain)[0].Model; lead != "llama-3.3-70b-versatile" {
		t.Fatalf("baseline auto routing picked %q", lead)
	}

	profile := UsageProfile{
		DislikedModels: map[string]int{"llama-3.3-70b-versatile": 2},
	}
	candidates := chain.CandidatesFor(Preference{Auto: true}, &profile)

	if candidates[0].Model == "llama-3.3-70b-versatile" {
		t.Fatal("a downvoted model should not lead the automatic routing")
	}
}

// TestCooldownBeatsPinnedModel makes sure a pin cannot keep a broken backend
// first in line.
func TestCooldownBeatsPinnedModel(t *testing.T) {
	chain := routingChain(t)

	for i := 0; i < cooldownAfter; i++ {
		chain.RecordFailure("gemini", errRateLimited)
	}

	candidates := chain.CandidatesFor(Preference{Model: "gemini-2.5-flash"}, nil)
	if candidates[0].Provider == "gemini" {
		t.Fatalf("a cooling down provider must not lead, got %+v", candidates[0])
	}

	found := false
	for _, candidate := range candidates {
		if candidate.Provider == "gemini" {
			found = true
		}
	}
	if !found {
		t.Error("the pinned model disappeared from the fallback list")
	}
}

func TestPreferenceDescribe(t *testing.T) {
	cases := map[string]struct {
		preference Preference
		want       string
	}{
		"auto":     {Preference{Auto: true}, "auto (best model for your usage)"},
		"provider": {Preference{Provider: "groq"}, "auto on groq"},
		"model":    {Preference{Model: "llama-3.3-70b-versatile"}, "llama-3.3-70b-versatile"},
	}

	for name, tc := range cases {
		if got := tc.preference.Describe(); got != tc.want {
			t.Errorf("%s: Describe() = %q, want %q", name, got, tc.want)
		}
	}
}

// TestApplyPresetFillsMissingFields is what allows "providers: - name: groq"
// to be a complete entry.
func TestApplyPresetFillsMissingFields(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "secret")

	cfg := ApplyPreset(Config{Name: "GROQ"})

	if cfg.BaseURL == "" {
		t.Error("BaseURL was not filled from the preset")
	}
	if cfg.APIKeyEnv != "GROQ_API_KEY" {
		t.Errorf("APIKeyEnv = %q, want GROQ_API_KEY", cfg.APIKeyEnv)
	}
	if cfg.APIKey != "secret" {
		t.Errorf("APIKey = %q, want it resolved from the environment", cfg.APIKey)
	}
	if !cfg.RequiresKey {
		t.Error("a hosted preset must require a key")
	}
	if len(cfg.Models) == 0 {
		t.Error("Models were not filled from the preset")
	}

	// Explicit values always win.
	explicit := ApplyPreset(Config{Name: "groq", BaseURL: "http://elsewhere/v1", Models: []string{"mine"}})
	if explicit.BaseURL != "http://elsewhere/v1" || len(explicit.Models) != 1 || explicit.Models[0] != "mine" {
		t.Errorf("ApplyPreset overwrote explicit values: %#v", explicit)
	}
}

// TestLocalPresetNeedsNoKey guards the local Ollama/LM Studio flow.
func TestLocalPresetNeedsNoKey(t *testing.T) {
	cfg := ApplyPreset(Config{Name: "ollama"})

	if cfg.RequiresKey {
		t.Error("a local endpoint must not require a key")
	}
	if !cfg.Configured() {
		t.Error("a local endpoint is configured by definition")
	}
}

// TestCostEndpointOnlyForOpenRouter pins the "who can report a price" rule:
// the lookup must follow the provider that answered, and only OpenRouter-style
// endpoints offer generation statistics.
func TestCostEndpointOnlyForOpenRouter(t *testing.T) {
	chain, err := NewChain([]Config{
		{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: "sk-or", RequiresKey: true, Models: []string{"m"}},
		{Name: "groq", BaseURL: "https://api.groq.com/openai/v1", APIKey: "gsk", RequiresKey: true, Models: []string{"m"}},
		{Name: "local", BaseURL: "http://localhost:11434/v1", Models: []string{"m"}},
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	baseURL, key, ok := chain.CostEndpoint("openrouter")
	if !ok || baseURL == "" || key != "sk-or" {
		t.Fatalf("CostEndpoint(openrouter) = %q, %q, %v", baseURL, key, ok)
	}

	for _, name := range []string{"groq", "local", "missing"} {
		if _, _, ok := chain.CostEndpoint(name); ok {
			t.Errorf("CostEndpoint(%s) should not report a price", name)
		}
	}
}
