package provider

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ModelInfo describes what a model is good at. Unknown models fall back to
// DefaultModelInfo, which is deliberately conservative.
type ModelInfo struct {
	Context   int     // context window in tokens
	Vision    bool    // accepts images
	Reasoning bool    // chain-of-thought style model, slower but stronger
	SizeB     float64 // parameter count in billions, 0 when unknown
	Known     bool    // true when this came from the catalogue, not a guess
}

// DefaultModelInfo is assumed for anything not in the catalogue. The context
// window is a conservative guess used for scoring only; Known stays false so
// the UI never presents a guess as a fact.
var DefaultModelInfo = ModelInfo{Context: 8192}

// charsPerToken is a rough English/romanised-Hindi conversion used to turn
// character counts into token estimates. Good enough for ranking.
const charsPerToken = 3.5

// catalogue maps a lower-cased model id to its capabilities.
var catalogue = map[string]ModelInfo{
	// Groq
	"llama-3.3-70b-versatile":     {Context: 131072, SizeB: 70},
	"llama-3.1-8b-instant":        {Context: 131072, SizeB: 8},
	"openai/gpt-oss-120b":         {Context: 131072, SizeB: 117, Reasoning: true},
	"openai/gpt-oss-20b":          {Context: 131072, SizeB: 21, Reasoning: true},
	"moonshotai/kimi-k2-instruct": {Context: 131072, SizeB: 1000, Reasoning: true},

	// Google
	"gemini-2.5-flash":      {Context: 1_048_576, Vision: true, SizeB: 0},
	"gemini-2.5-flash-lite": {Context: 1_048_576, Vision: true, SizeB: 0},
	"gemini-2.0-flash":      {Context: 1_048_576, Vision: true, SizeB: 0},

	// OpenRouter
	"deepseek/deepseek-r1:free":           {Context: 131072, Reasoning: true, SizeB: 671},
	"deepseek/deepseek-chat-v3-0324:free": {Context: 131072, SizeB: 671},
	"openrouter/free":                     {Context: 200000},
	"google/gemma-3-27b-it:free":          {Context: 131072, Vision: true, SizeB: 27},
	"qwen/qwen3-coder:free":               {Context: 262144, SizeB: 480},

	// NVIDIA NIM
	"meta/llama-3.3-70b-instruct":            {Context: 131072, SizeB: 70},
	"nvidia/llama-3.3-nemotron-super-49b-v1": {Context: 131072, SizeB: 49, Reasoning: true},
	"qwen/qwen3-235b-a22b":                   {Context: 32768, SizeB: 235},

	// Cerebras
	"llama-3.3-70b": {Context: 65536, SizeB: 70},
	"gpt-oss-120b":  {Context: 65536, SizeB: 117, Reasoning: true},

	// Ollama / local
	"llama3.2:3b":         {Context: 131072, SizeB: 3},
	"llama3.2:1b":         {Context: 131072, SizeB: 1},
	"llama3.2-vision:11b": {Context: 131072, Vision: true, SizeB: 11},
	"qwen2.5:7b":          {Context: 32768, SizeB: 7},
}

// providerSpeed hints how quickly a provider answers. Used to favour fast
// backends for chatty users. 0..1, higher is faster.
var providerSpeed = map[string]float64{
	"groq":       1.0,
	"cerebras":   0.95,
	"gemini":     0.7,
	"openrouter": 0.5,
	"nvidia":     0.5,
	"ollama":     0.3,
	"local":      0.3,
}

// UsageProfile summarises how a user actually uses the bot.
type UsageProfile struct {
	RequestsPerDay   float64
	AvgPromptChars   float64
	HistoryMessages  int
	PeakHistoryChars int
	UsesVision       bool
	FailedRecently   int
}

// Recommendation is the model the bot thinks suits the user right now.
type Recommendation struct {
	Provider string
	Model    string
	Reason   string
	Score    float64
}

// LookupModel returns known capabilities for a model id.
func LookupModel(model string) ModelInfo {
	if info, ok := catalogue[strings.ToLower(strings.TrimSpace(model))]; ok {
		info.Known = true

		return info
	}

	return DefaultModelInfo
}

// Recommend scores every available (provider, model) pair against the usage
// profile and returns the best one with a human readable justification.
func (c *Chain) Recommend(profile UsageProfile) (Recommendation, error) {
	candidates := c.Candidates()
	if len(candidates) == 0 {
		return Recommendation{}, fmt.Errorf("no providers configured")
	}

	status := c.Status()
	cooldowns := make(map[string]bool, len(status))
	for _, s := range status {
		cooldowns[s.Name] = s.Cooldown > 0
	}

	active := c.ActiveName()

	needTokens := (float64(profile.HistoryMessages)*profile.AvgPromptChars +
		float64(profile.PeakHistoryChars)) / charsPerToken

	type scored struct {
		candidate Candidate
		score     float64
		reasons   []string
	}

	results := make([]scored, 0, len(candidates))

	for _, candidate := range candidates {
		info := LookupModel(candidate.Model)
		score := 0.0

		var reasons []string

		// Context window fit.
		switch {
		case needTokens <= 0:
			// No history yet: anything works.
		case float64(info.Context) >= needTokens*1.5:
			score += 30
			reasons = append(reasons, "plenty of context headroom")
		case float64(info.Context) >= needTokens:
			score += 15
			reasons = append(reasons, "fits your conversation length")
		default:
			score -= 40
			reasons = append(reasons, "context window is tight for your history")
		}

		// Vision is a hard requirement when it is used.
		if profile.UsesVision {
			if info.Vision {
				score += 40
				reasons = append(reasons, "handles images")
			} else {
				score -= 100
			}
		}

		// Volume: chatty users want throughput, quiet users want quality.
		speed := providerSpeed[strings.ToLower(candidate.Provider)]

		switch {
		case profile.RequestsPerDay > 50:
			score += speed * 25
			if info.SizeB > 0 && info.SizeB <= 20 {
				score += 10
				reasons = append(reasons, "small and fast for high volume")
			}
		case profile.RequestsPerDay < 10:
			if info.SizeB >= 70 {
				score += 20
				reasons = append(reasons, "larger model for better answers")
			}
			score += speed * 5
		default:
			score += speed * 15
		}

		// Reasoning models help on long, involved prompts.
		if info.Reasoning && profile.AvgPromptChars > 800 {
			score += 10
			reasons = append(reasons, "reasoning model suits long prompts")
		}

		// Prefer the current provider so we do not churn for no reason.
		if strings.EqualFold(candidate.Provider, active) {
			score += 5
		}

		// Avoid backends that are currently failing.
		if cooldowns[candidate.Provider] {
			score -= 60
		}
		if profile.FailedRecently > 0 {
			score -= math.Min(float64(profile.FailedRecently)*5, 20)
		}

		results = append(results, scored{candidate: candidate, score: score, reasons: reasons})
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	best := results[0]

	reason := strings.Join(best.reasons, ", ")
	if reason == "" {
		reason = "best overall match for your current usage"
	}

	return Recommendation{
		Provider: best.candidate.Provider,
		Model:    best.candidate.Model,
		Reason:   reason,
		Score:    best.score,
	}, nil
}

// DescribeModel renders a one line capability summary for /models.
func DescribeModel(model string) string {
	info := LookupModel(model)
	if !info.Known {
		// Never present a guessed capability as a fact.
		return ""
	}

	var parts []string

	if info.Context >= 1_000_000 {
		parts = append(parts, fmt.Sprintf("%.1fM ctx", float64(info.Context)/1_000_000))
	} else if info.Context >= 1000 {
		parts = append(parts, fmt.Sprintf("%dK ctx", info.Context/1000))
	}
	if info.Vision {
		parts = append(parts, "vision")
	}
	if info.Reasoning {
		parts = append(parts, "reasoning")
	}
	if info.SizeB > 0 {
		parts = append(parts, fmt.Sprintf("%gB", info.SizeB))
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, " · ")
}
