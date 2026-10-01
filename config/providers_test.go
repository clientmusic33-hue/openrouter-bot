package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"

	"openrouter-bot/provider"
)

// TestLoadProvidersFromYAML is the check that matters for the provider chain:
// viper lower-cases every key, so the struct tags in provider.Config have to
// match the snake_case keys in config.yaml. If they drift, the chain silently
// falls back to the flat environment variables.
func TestLoadProvidersFromYAML(t *testing.T) {
	dir := t.TempDir()

	yaml := `
type: openrouter
model: some/model
base_url: https://example.invalid/v1

providers:
  - name: openrouter
    base_url: https://openrouter.ai/api/v1
    api_key_env: API_KEY
    models:
      - deepseek/deepseek-r1:free
      - openrouter/free
  - name: groq
    base_url: https://api.groq.com/openai/v1
    api_key_env: GROQ_API_KEY
    models:
      - llama-3.3-70b-versatile
`

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	t.Setenv("API_KEY", "or-secret")
	t.Setenv("GROQ_API_KEY", "groq-secret")

	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.SetConfigFile(path)
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("ReadInConfig: %v", err)
	}

	providers := loadProviders()
	if len(providers) != 2 {
		t.Fatalf("loadProviders() returned %d entries, want 2: %#v", len(providers), providers)
	}

	if providers[0].Name != "openrouter" {
		t.Errorf("providers[0].Name = %q", providers[0].Name)
	}
	if providers[0].BaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("providers[0].BaseURL = %q", providers[0].BaseURL)
	}
	if providers[0].APIKey != "or-secret" {
		t.Errorf("providers[0].APIKey = %q, want it resolved from API_KEY", providers[0].APIKey)
	}
	if len(providers[0].Models) != 2 {
		t.Errorf("providers[0].Models = %v", providers[0].Models)
	}

	if providers[1].Name != "groq" || providers[1].APIKey != "groq-secret" {
		t.Errorf("providers[1] = %#v", providers[1])
	}
	if len(providers[1].Models) != 1 || providers[1].Models[0] != "llama-3.3-70b-versatile" {
		t.Errorf("providers[1].Models = %v", providers[1].Models)
	}
}

func TestLoadProvidersMissingKeyStaysListed(t *testing.T) {
	dir := t.TempDir()

	yaml := `
providers:
  - name: groq
    base_url: https://api.groq.com/openai/v1
    api_key_env: GROQ_API_KEY_THAT_IS_NOT_SET
    models:
      - llama-3.3-70b-versatile
`

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.SetConfigFile(path)
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("ReadInConfig: %v", err)
	}

	providers := loadProviders()
	if len(providers) != 1 {
		t.Fatalf("want the provider listed even without a key, got %d", len(providers))
	}
	if providers[0].APIKey != "" {
		t.Errorf("APIKey = %q, want empty", providers[0].APIKey)
	}
}

func TestLoadProvidersAbsent(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("model: x\n"), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.SetConfigFile(path)
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("ReadInConfig: %v", err)
	}

	if providers := loadProviders(); providers != nil {
		t.Errorf("loadProviders() = %#v, want nil so the flat fallback is used", providers)
	}
}

func TestFallbackProviders(t *testing.T) {
	for _, name := range extraProviderOrder {
		preset, ok := provider.LookupPreset(name)
		if ok && preset.APIKeyEnv != "" {
			t.Setenv(preset.APIKeyEnv, "")
		}
	}

	conf := &Config{
		OpenAIBaseURL: "https://openrouter.ai/api/v1",
		OpenAIApiKey:  "or-key",
		Model:         ModelParameters{ModelName: "some/model"},
	}

	providers := FallbackProviders(conf)
	if len(providers) != 1 {
		t.Fatalf("want only the primary provider without GEMINI_API_KEY, got %d", len(providers))
	}
	if providers[0].Name != "openrouter" || providers[0].APIKey != "or-key" {
		t.Errorf("providers[0] = %#v", providers[0])
	}

	conf.GeminiAPIKey = "gemini-key"
	providers = FallbackProviders(conf)
	if len(providers) != 2 {
		t.Fatalf("want the Gemini fallback included, got %d", len(providers))
	}
	if providers[1].Name != "gemini" || providers[1].APIKey != "gemini-key" {
		t.Errorf("providers[1] = %#v", providers[1])
	}
}

// TestPublicModeOpensEverything pins the behaviour of the single switch.
func TestPublicModeOpensEverything(t *testing.T) {
	c := &Config{
		BudgetPeriod:       "monthly",
		UserBudget:         1,
		GuestBudget:        0,
		RateLimitPerMinute: 10,
		GroupChatMode:      GroupModeMention,
		PublicMode:         true,
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if c.UserBudget >= 0 || c.GuestBudget >= 0 {
		t.Errorf("public mode must make budgets unlimited (negative), got user=%v guest=%v",
			c.UserBudget, c.GuestBudget)
	}
	if c.RateLimitPerMinute != 0 {
		t.Errorf("RateLimitPerMinute = %d, want 0", c.RateLimitPerMinute)
	}
	if c.GroupChatMode != GroupModeAll {
		t.Errorf("GroupChatMode = %q, want %q", c.GroupChatMode, GroupModeAll)
	}
}

// TestNvidiaNeedsOnlyANameAndItsKey pins the NVIDIA NIM contract: the preset
// name and NVIDIA_API_KEY are all a user has to provide, because the endpoint
// and the models come from the built-in catalogue. It also pins the other
// half of the deal - without a key the provider stays listed, so /providers
// can explain why it is skipped, but never serves a request.
func TestNvidiaNeedsOnlyANameAndItsKey(t *testing.T) {
	const yaml = "providers:\n  - name: nvidia\n"

	load := func(t *testing.T, key string) []provider.Config {
		t.Helper()

		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatalf("writing config: %v", err)
		}

		t.Setenv("NVIDIA_API_KEY", key)

		viper.Reset()
		t.Cleanup(viper.Reset)

		viper.SetConfigFile(path)
		viper.AutomaticEnv()

		if err := viper.ReadInConfig(); err != nil {
			t.Fatalf("ReadInConfig: %v", err)
		}

		return loadProviders()
	}

	t.Run("with a key it is ready to answer", func(t *testing.T) {
		providers := load(t, "nvapi-test")

		if len(providers) != 1 {
			t.Fatalf("loadProviders() returned %d entries, want 1: %#v", len(providers), providers)
		}

		got := providers[0]

		if got.Name != "nvidia" {
			t.Errorf("Name = %q, want nvidia", got.Name)
		}
		if got.BaseURL != "https://integrate.api.nvidia.com/v1" {
			t.Errorf("BaseURL = %q, want the NVIDIA NIM endpoint", got.BaseURL)
		}
		if got.APIKeyEnv != "NVIDIA_API_KEY" {
			t.Errorf("APIKeyEnv = %q, want NVIDIA_API_KEY", got.APIKeyEnv)
		}
		if got.APIKey != "nvapi-test" {
			t.Errorf("APIKey = %q, want it resolved from NVIDIA_API_KEY", got.APIKey)
		}
		if len(got.Models) == 0 {
			t.Error("Models is empty, want the preset defaults")
		}
		if !got.Configured() {
			t.Error("Configured() = false, want true once the key is set")
		}
	})

	t.Run("without a key it is listed but never used", func(t *testing.T) {
		providers := load(t, "")

		if len(providers) != 1 {
			t.Fatalf("loadProviders() returned %d entries, want 1 so /providers can explain", len(providers))
		}
		if providers[0].APIKey != "" {
			t.Errorf("APIKey = %q, want empty", providers[0].APIKey)
		}
		if providers[0].Configured() {
			t.Error("Configured() = true without a key, want false")
		}
	})
}

func TestHuggingFaceAndMistralInitializeFromConfig(t *testing.T) {
	const yaml = `providers:
  - name: huggingface
  - name: mistral
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	t.Setenv("HF_TOKEN", "hf-test-token")
	t.Setenv("MISTRAL_API_KEY", "mistral-test-key")

	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.SetConfigFile(path)
	viper.AutomaticEnv()
	if err := viper.ReadInConfig(); err != nil {
		t.Fatalf("ReadInConfig: %v", err)
	}

	configs := loadProviders()
	if len(configs) != 2 {
		t.Fatalf("loadProviders() returned %d entries, want 2", len(configs))
	}

	want := map[string]struct {
		baseURL string
		keyEnv  string
		key     string
	}{
		"huggingface": {"https://router.huggingface.co/v1", "HF_TOKEN", "hf-test-token"},
		"mistral":     {"https://api.mistral.ai/v1", "MISTRAL_API_KEY", "mistral-test-key"},
	}
	for _, cfg := range configs {
		expected, ok := want[cfg.Name]
		if !ok {
			t.Fatalf("unexpected provider %q", cfg.Name)
		}
		if cfg.BaseURL != expected.baseURL || cfg.APIKeyEnv != expected.keyEnv || cfg.APIKey != expected.key {
			t.Errorf("%s configuration has incorrect endpoint or key resolution", cfg.Name)
		}
		if !cfg.Configured() || len(cfg.Models) == 0 {
			t.Errorf("%s is not ready with a key and preset model", cfg.Name)
		}
		if _, err := provider.NewProvider(cfg); err != nil {
			t.Errorf("NewProvider(%s): %v", cfg.Name, err)
		}
	}

	chain, err := provider.NewChain(configs)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	candidates := chain.Candidates()
	found := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		found[candidate.Provider] = true
	}
	for name := range want {
		if !found[name] {
			t.Errorf("%s has no selectable candidate", name)
		}
	}
}

func TestProvidersFromEnvIncludesHuggingFaceAndMistral(t *testing.T) {
	for _, name := range extraProviderOrder {
		preset, ok := provider.LookupPreset(name)
		if ok && preset.APIKeyEnv != "" {
			t.Setenv(preset.APIKeyEnv, "")
		}
	}
	t.Setenv("HF_TOKEN", "hf-test-token")
	t.Setenv("MISTRAL_API_KEY", "mistral-test-key")

	configs := ProvidersFromEnv()
	found := make(map[string]provider.Config, len(configs))
	for _, cfg := range configs {
		found[cfg.Name] = cfg
	}
	for _, name := range []string{"huggingface", "mistral"} {
		cfg, ok := found[name]
		if !ok || !cfg.Configured() {
			t.Errorf("%s was not included as a configured environment fallback", name)
		}
	}
}
