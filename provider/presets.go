package provider

import (
	"os"
	"strings"
)

// Preset describes a well known OpenAI-compatible backend.
//
// Presets exist so that adding a provider is a one line change in config.yaml:
// a name alone is enough to get a working base URL, the right environment
// variable for the key and a sensible starting list of models.
type Preset struct {
	Name string
	// BaseURL is the OpenAI-compatible endpoint, including /v1.
	BaseURL string
	// APIKeyEnv names the environment variable holding the key. An empty
	// value marks a local endpoint that does not use a key at all.
	APIKeyEnv string
	// Models are used when the config file does not list any.
	Models []string
	// Docs is shown next to the provider in the UI.
	Docs string
}

// RequiresKey reports whether the preset needs a credential to answer.
func (p Preset) RequiresKey() bool { return p.APIKeyEnv != "" }

// presets holds every backend the bot knows out of the box. Order matters:
// it is the fallback chain used when config.yaml has no providers block.
var presets = []Preset{
	{
		Name:      "openrouter",
		BaseURL:   "https://openrouter.ai/api/v1",
		APIKeyEnv: "API_KEY",
		Models: []string{
			"deepseek/deepseek-r1:free",
			"openrouter/free",
			"meta-llama/llama-3.3-70b-instruct:free",
			"google/gemma-3-27b-it:free",
			"qwen/qwen3-coder:free",
		},
		Docs: "https://openrouter.ai/models?max_price=0",
	},
	{
		Name:      "groq",
		BaseURL:   "https://api.groq.com/openai/v1",
		APIKeyEnv: "GROQ_API_KEY",
		Models: []string{
			"llama-3.3-70b-versatile",
			"openai/gpt-oss-120b",
			"openai/gpt-oss-20b",
			"llama-3.1-8b-instant",
		},
		Docs: "https://console.groq.com/keys",
	},
	{
		Name:      "gemini",
		BaseURL:   "https://generativelanguage.googleapis.com/v1beta/openai/",
		APIKeyEnv: "GEMINI_API_KEY",
		Models: []string{
			"gemini-2.5-flash",
			"gemini-2.5-flash-lite",
			"gemini-2.0-flash",
		},
		Docs: "https://aistudio.google.com/apikey",
	},
	{
		Name:      "cerebras",
		BaseURL:   "https://api.cerebras.ai/v1",
		APIKeyEnv: "CEREBRAS_API_KEY",
		Models: []string{
			"gpt-oss-120b",
			"llama-3.3-70b",
		},
		Docs: "https://cloud.cerebras.ai",
	},
	{
		Name:      "nvidia",
		BaseURL:   "https://integrate.api.nvidia.com/v1",
		APIKeyEnv: "NVIDIA_API_KEY",
		// The catalogue at build.nvidia.com holds more than a hundred
		// models; these are the ones a Telegram bot reaches for. Any other
		// model id from the catalogue can be listed under models: in
		// config.yaml, and the failover walks them top to bottom.
		Models: []string{
			"nvidia/nemotron-3.5-lightning-30b-a3b",
			"meta/llama-3.3-70b-instruct",
			"deepseek-ai/deepseek-v4-pro",
			"openai/gpt-oss-120b",
			"google/gemma-4-31b-it",
			"moonshotai/kimi-k3",
		},
		Docs: "https://build.nvidia.com/models",
	},
	{
		Name:      "mistral",
		BaseURL:   "https://api.mistral.ai/v1",
		APIKeyEnv: "MISTRAL_API_KEY",
		Models: []string{
			"mistral-small-latest",
			"open-mistral-nemo",
		},
		Docs: "https://console.mistral.ai",
	},
	{
		Name:      "deepseek",
		BaseURL:   "https://api.deepseek.com/v1",
		APIKeyEnv: "DEEPSEEK_API_KEY",
		Models: []string{
			"deepseek-chat",
			"deepseek-reasoner",
		},
		Docs: "https://platform.deepseek.com",
	},
	{
		Name:      "together",
		BaseURL:   "https://api.together.xyz/v1",
		APIKeyEnv: "TOGETHER_API_KEY",
		Models: []string{
			"meta-llama/Llama-3.3-70B-Instruct-Turbo",
		},
		Docs: "https://api.together.ai/settings/api-keys",
	},
	{
		Name:      "ollama",
		BaseURL:   "http://localhost:11434/v1",
		APIKeyEnv: "",
		Models:    []string{"llama3.2:3b"},
		Docs:      "https://ollama.com",
	},
	{
		Name:      "lmstudio",
		BaseURL:   "http://localhost:1234/v1",
		APIKeyEnv: "",
		Models:    []string{"local-model"},
		Docs:      "https://lmstudio.ai",
	},
}

// presetIndex is built once so lookups stay allocation free.
var presetIndex = func() map[string]Preset {
	index := make(map[string]Preset, len(presets))
	for _, preset := range presets {
		index[preset.Name] = preset
	}

	return index
}()

// LookupPreset finds a preset by name. Matching is case insensitive because
// people write "Groq" as often as "groq".
func LookupPreset(name string) (Preset, bool) {
	preset, ok := presetIndex[strings.ToLower(strings.TrimSpace(name))]

	return preset, ok
}

// Presets returns a copy of the built-in catalogue in chain order.
func Presets() []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)

	return out
}

// PresetNames lists the built-in provider names.
func PresetNames() []string {
	names := make([]string, 0, len(presets))
	for _, preset := range presets {
		names = append(names, preset.Name)
	}

	return names
}

// ApplyPreset fills in every field the operator left out. A provider that is
// only named in config.yaml therefore works immediately, while an explicit
// base_url or model list always wins.
func ApplyPreset(cfg Config) Config {
	preset, ok := LookupPreset(cfg.Name)
	if !ok {
		return cfg
	}

	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = preset.BaseURL
	}
	if strings.TrimSpace(cfg.APIKeyEnv) == "" {
		cfg.APIKeyEnv = preset.APIKeyEnv
	}
	if len(cfg.Models) == 0 {
		cfg.Models = append([]string(nil), preset.Models...)
	}
	if strings.TrimSpace(cfg.APIKey) == "" && cfg.APIKeyEnv != "" {
		cfg.APIKey = os.Getenv(cfg.APIKeyEnv)
	}

	// A provider that needs a key and has none can never answer. Keeping that
	// fact on the struct lets the chain skip it instead of paying a failed
	// round trip on every request.
	cfg.RequiresKey = cfg.RequiresKey || preset.RequiresKey() || strings.TrimSpace(cfg.APIKeyEnv) != ""

	return cfg
}

// providerSpeed hints how quickly a provider answers. Used to favour fast
// backends for chatty users. 0..1, higher is faster.
var providerSpeed = map[string]float64{
	"groq":       1.0,
	"cerebras":   0.95,
	"gemini":     0.7,
	"mistral":    0.7,
	"openrouter": 0.5,
	"nvidia":     0.5,
	"deepseek":   0.5,
	"together":   0.5,
	"ollama":     0.3,
	"lmstudio":   0.3,
	"local":      0.3,
}

// Speed returns the relative speed hint for a provider, defaulting to a
// middle-of-the-road value so unknown backends are neither favoured nor
// penalised.
func Speed(name string) float64 {
	if speed, ok := providerSpeed[strings.ToLower(strings.TrimSpace(name))]; ok {
		return speed
	}

	return 0.5
}
