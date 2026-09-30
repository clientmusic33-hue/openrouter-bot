package provider

import (
	"errors"
	"strings"
	"testing"
)

func testConfigs() []Config {
	return []Config{
		{
			Name:    "openrouter",
			BaseURL: "https://openrouter.ai/api/v1",
			APIKey:  "or-key",
			Models:  []string{"deepseek/deepseek-r1:free", "openrouter/free"},
		},
		{
			Name:    "groq",
			BaseURL: "https://api.groq.com/openai/v1",
			APIKey:  "groq-key",
			Models:  []string{"llama-3.3-70b-versatile", "llama-3.1-8b-instant"},
		},
		{
			Name:    "ollama",
			BaseURL: "http://localhost:11434/v1",
			Models:  []string{"llama3.2:3b"},
		},
	}
}

func newTestChain(t *testing.T) *Chain {
	t.Helper()

	chain, err := NewChain(testConfigs())
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	return chain
}

// -----------------------------------------------------------------------------
// Construction
// -----------------------------------------------------------------------------

func TestNewChainRequiresProviders(t *testing.T) {
	if _, err := NewChain(nil); err == nil {
		t.Fatal("NewChain should reject an empty list")
	}
}

func TestNewChainValidatesEntries(t *testing.T) {
	cases := map[string][]Config{
		"missing name":    {{BaseURL: "http://x", Models: []string{"m"}}},
		"missing baseURL": {{Name: "x", Models: []string{"m"}}},
		"missing models":  {{Name: "x", BaseURL: "http://x"}},
	}

	for name, cfgs := range cases {
		if _, err := NewChain(cfgs); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNewChainDefaultsModel(t *testing.T) {
	chain := newTestChain(t)

	if got := chain.Model(); got != "deepseek/deepseek-r1:free" {
		t.Errorf("Model() = %q, want the first model of the first provider", got)
	}
}

// -----------------------------------------------------------------------------
// Candidate ordering - this is the failover path
// -----------------------------------------------------------------------------

func TestCandidatesStartWithActiveProviderModel(t *testing.T) {
	chain := newTestChain(t)

	candidates := chain.Candidates()

	if len(candidates) == 0 {
		t.Fatal("no candidates")
	}
	if candidates[0].Provider != "openrouter" || candidates[0].Model != "deepseek/deepseek-r1:free" {
		t.Errorf("first candidate = %+v, want openrouter/deepseek-r1:free", candidates[0])
	}
}

func TestCandidatesCoverEveryModel(t *testing.T) {
	chain := newTestChain(t)

	candidates := chain.Candidates()

	if len(candidates) != 5 {
		t.Fatalf("got %d candidates, want 5", len(candidates))
	}

	seen := map[string]bool{}
	for _, candidate := range candidates {
		key := candidate.Provider + "/" + candidate.Model
		if seen[key] {
			t.Errorf("duplicate candidate %s", key)
		}
		seen[key] = true
	}
}

// TestCandidatesModelFirst ensures a pinned model is tried before anything
// else, even when it belongs to a later provider.
func TestCandidatesModelFirst(t *testing.T) {
	chain := newTestChain(t)

	if err := chain.SetModel("llama-3.1-8b-instant"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	candidates := chain.Candidates()

	if candidates[0].Model != "llama-3.1-8b-instant" {
		t.Errorf("first candidate model = %q, want the pinned model", candidates[0].Model)
	}
	if candidates[0].Provider != "groq" {
		t.Errorf("first candidate provider = %q, want groq", candidates[0].Provider)
	}
	// The active provider should follow the pinned model.
	if chain.ActiveName() != "groq" {
		t.Errorf("ActiveName() = %q, want groq", chain.ActiveName())
	}
}

// TestSetModelWithUnknownModel keeps the model selectable rather than
// silently dropping an operator's choice.
func TestSetModelWithUnknownModel(t *testing.T) {
	chain := newTestChain(t)

	if err := chain.SetModel("some/unlisted-model"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if got := chain.Model(); got != "some/unlisted-model" {
		t.Errorf("Model() = %q", got)
	}
}

func TestSetActive(t *testing.T) {
	chain := newTestChain(t)

	name, err := chain.SetActive("groq")
	if err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if name != "groq" {
		t.Errorf("SetActive returned %q", name)
	}
	if got := chain.ActiveName(); got != "groq" {
		t.Errorf("ActiveName() = %q, want groq", got)
	}
	// Switching provider must move the model selection too.
	if got := chain.Model(); got != "llama-3.3-70b-versatile" {
		t.Errorf("Model() = %q, want groq's first model", got)
	}
}

func TestSetActiveUnknown(t *testing.T) {
	chain := newTestChain(t)

	if _, err := chain.SetActive("nope"); err == nil {
		t.Error("SetActive should reject an unknown provider")
	}
}

func TestResetModel(t *testing.T) {
	chain := newTestChain(t)

	if err := chain.SetModel("llama3.2:3b"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if got := chain.ResetModel(); got != "llama3.2:3b" {
		t.Errorf("ResetModel() = %q, want the active provider's first model", got)
	}
}

// -----------------------------------------------------------------------------
// Health and cooldown
// -----------------------------------------------------------------------------

// TestCooldownDemotesFailingProvider is the core failover guarantee: a
// provider that keeps failing must stop being tried first.
func TestCooldownDemotesFailingProvider(t *testing.T) {
	chain := newTestChain(t)

	for i := 0; i < cooldownAfter; i++ {
		chain.RecordFailure("openrouter", errors.New("429 rate limited"))
	}

	candidates := chain.Candidates()

	if candidates[0].Provider == "openrouter" {
		t.Fatalf("a cooled-down provider must not lead the chain, got %+v", candidates[0])
	}

	// It must still be present so the chain never runs out of options.
	found := false
	for _, candidate := range candidates {
		if candidate.Provider == "openrouter" {
			found = true
		}
	}
	if !found {
		t.Error("the failing provider disappeared from the chain")
	}

	// Recovery restores the original ordering.
	chain.RecordSuccess("openrouter")
	if candidates := chain.Candidates(); candidates[0].Provider != "openrouter" {
		t.Errorf("after recovery openrouter should lead again, got %q", candidates[0].Provider)
	}
}

func TestRecordSuccessClearsFailures(t *testing.T) {
	chain := newTestChain(t)

	chain.RecordFailure("groq", errors.New("boom"))
	chain.RecordSuccess("groq")

	infos := chain.Status()
	for _, info := range infos {
		if info.Name == "groq" {
			if info.Failures != 0 {
				t.Errorf("Failures = %d, want 0", info.Failures)
			}
			if info.Successes != 1 {
				t.Errorf("Successes = %d, want 1", info.Successes)
			}
			if info.LastErr != "" {
				t.Errorf("LastErr = %q, want empty", info.LastErr)
			}
		}
	}
}

func TestStatusReportsMissingKey(t *testing.T) {
	chain := newTestChain(t)

	for _, info := range chain.Status() {
		if info.Name == "ollama" && info.Configured {
			t.Error("ollama has no API key, Configured should be false")
		}
		if info.Name == "groq" && !info.Configured {
			t.Error("groq has an API key, Configured should be true")
		}
	}
}

// -----------------------------------------------------------------------------
// Recommendations
// -----------------------------------------------------------------------------

func TestRecommendPrefersVisionWhenUsed(t *testing.T) {
	chain, err := NewChain([]Config{
		{
			Name:    "groq",
			BaseURL: "https://api.groq.com/openai/v1",
			Models:  []string{"llama-3.3-70b-versatile"},
		},
		{
			Name:    "gemini",
			BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/",
			Models:  []string{"gemini-2.5-flash"},
		},
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	recommendation, err := chain.Recommend(UsageProfile{UsesVision: true})
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if recommendation.Model != "gemini-2.5-flash" {
		t.Errorf("Recommended %q, want a vision capable model", recommendation.Model)
	}
	if !strings.Contains(recommendation.Reason, "images") {
		t.Errorf("Reason = %q, should mention images", recommendation.Reason)
	}
}

func TestRecommendPrefersFastProviderForHighVolume(t *testing.T) {
	chain, err := NewChain([]Config{
		{
			Name:    "ollama",
			BaseURL: "http://localhost:11434/v1",
			Models:  []string{"llama3.2:3b"},
		},
		{
			Name:    "groq",
			BaseURL: "https://api.groq.com/openai/v1",
			Models:  []string{"llama-3.3-70b-versatile"},
		},
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	recommendation, err := chain.Recommend(UsageProfile{RequestsPerDay: 200})
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if recommendation.Provider != "groq" {
		t.Errorf("Recommended provider %q, want the fast one for high volume", recommendation.Provider)
	}
}

func TestRecommendAccountsForContextNeed(t *testing.T) {
	chain, err := NewChain([]Config{
		{
			Name:    "small",
			BaseURL: "http://small/v1",
			Models:  []string{"tiny-model"},
		},
		{
			Name:    "big",
			BaseURL: "http://big/v1",
			Models:  []string{"gemini-2.5-flash"},
		},
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	// 60 messages of ~2000 chars needs far more than an 8K default window.
	recommendation, err := chain.Recommend(UsageProfile{
		HistoryMessages:  60,
		AvgPromptChars:   2000,
		PeakHistoryChars: 5000,
	})
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if recommendation.Model != "gemini-2.5-flash" {
		t.Errorf("Recommended %q, want the large context model", recommendation.Model)
	}
}

func TestRecommendWithoutProviders(t *testing.T) {
	chain := &Chain{health: map[string]*health{}}

	if _, err := chain.Recommend(UsageProfile{}); err == nil {
		t.Error("Recommend should fail with no candidates")
	}
}

func TestDescribeModel(t *testing.T) {
	if got := DescribeModel("gemini-2.5-flash"); !strings.Contains(got, "vision") {
		t.Errorf("DescribeModel(gemini-2.5-flash) = %q, want vision", got)
	}
	if got := DescribeModel("unknown-thing"); got != "" {
		t.Errorf("DescribeModel(unknown) = %q, want empty", got)
	}
}

func TestProviderOf(t *testing.T) {
	chain := newTestChain(t)

	name, ok := chain.ProviderOf("llama-3.3-70b-versatile")
	if !ok || name != "groq" {
		t.Errorf("ProviderOf = %q, %v; want groq, true", name, ok)
	}
	if _, ok := chain.ProviderOf("nope"); ok {
		t.Error("ProviderOf should report false for an unknown model")
	}
}
