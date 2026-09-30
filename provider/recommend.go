package provider

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
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
	"llama-3.3-70b-versatile":                   {Context: 131072, SizeB: 70},
	"llama-3.1-8b-instant":                      {Context: 131072, SizeB: 8},
	"llama-3.1-70b-versatile":                   {Context: 131072, SizeB: 70},
	"openai/gpt-oss-120b":                       {Context: 131072, SizeB: 117, Reasoning: true},
	"openai/gpt-oss-20b":                        {Context: 131072, SizeB: 21, Reasoning: true},
	"moonshotai/kimi-k2-instruct":               {Context: 131072, SizeB: 1000, Reasoning: true},
	"qwen/qwen3-32b":                            {Context: 131072, SizeB: 32, Reasoning: true},
	"meta-llama/llama-4-scout-17b-16e-instruct": {Context: 131072, SizeB: 17, Vision: true},

	// Google
	"gemini-2.5-flash":      {Context: 1_048_576, Vision: true},
	"gemini-2.5-flash-lite": {Context: 1_048_576, Vision: true},
	"gemini-2.5-pro":        {Context: 1_048_576, Vision: true, Reasoning: true},
	"gemini-2.0-flash":      {Context: 1_048_576, Vision: true},

	// OpenRouter
	"deepseek/deepseek-r1:free":                     {Context: 131072, Reasoning: true, SizeB: 671},
	"deepseek/deepseek-r1":                          {Context: 131072, Reasoning: true, SizeB: 671},
	"deepseek/deepseek-chat-v3-0324:free":           {Context: 131072, SizeB: 671},
	"deepseek/deepseek-chat":                        {Context: 131072, SizeB: 671},
	"openrouter/free":                               {Context: 200000},
	"openrouter/auto":                               {Context: 200000},
	"google/gemma-3-27b-it:free":                    {Context: 131072, Vision: true, SizeB: 27},
	"qwen/qwen3-coder:free":                         {Context: 262144, SizeB: 480},
	"meta-llama/llama-3.3-70b-instruct:free":        {Context: 131072, SizeB: 70},
	"mistralai/mistral-small-3.2-24b-instruct:free": {Context: 131072, SizeB: 24, Vision: true},

	// NVIDIA NIM
	"meta/llama-3.3-70b-instruct":            {Context: 131072, SizeB: 70},
	"nvidia/llama-3.3-nemotron-super-49b-v1": {Context: 131072, SizeB: 49, Reasoning: true},
	"qwen/qwen3-235b-a22b":                   {Context: 32768, SizeB: 235},

	// Cerebras
	"llama-3.3-70b": {Context: 65536, SizeB: 70},
	"gpt-oss-120b":  {Context: 65536, SizeB: 117, Reasoning: true},

	// Mistral
	"mistral-small-latest": {Context: 131072, SizeB: 24, Vision: true},
	"open-mistral-nemo":    {Context: 131072, SizeB: 12},
	"mistral-large-latest": {Context: 131072, SizeB: 123},

	// Together
	"meta-llama/llama-3.3-70b-instruct-turbo":           {Context: 131072, SizeB: 70},
	"meta-llama/llama-4-maverick-17b-128e-instruct-fp8": {Context: 131072, SizeB: 17, Vision: true},

	// DeepSeek direct
	"deepseek-reasoner": {Context: 65536, Reasoning: true, SizeB: 671},

	// Ollama / local
	"llama3.2:3b":         {Context: 131072, SizeB: 3},
	"llama3.2:1b":         {Context: 131072, SizeB: 1},
	"llama3.2-vision:11b": {Context: 131072, Vision: true, SizeB: 11},
	"qwen2.5:7b":          {Context: 32768, SizeB: 7},
	"local-model":         {Context: 32768},
}

// UsageProfile summarises how a user actually uses the bot. It drives both the
// automatic routing and the /recommend answer.
type UsageProfile struct {
	RequestsPerDay   float64
	AvgPromptChars   float64
	HistoryMessages  int
	PeakHistoryChars int
	UsesVision       bool
	FailedRecently   int
	// DislikedModels counts the answers the user downvoted, per model. A
	// model the user already rejected is pushed down the list.
	DislikedModels map[string]int
	// NeedsReasoning is set when the user asks long, involved questions.
	NeedsReasoning bool
	// PrefersFast is set for heavy users, who care about latency.
	PrefersFast bool
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

// Scoring weights. Kept as constants so the ranking is easy to reason about
// and easy to test.
const (
	scoreContextRoomy   = 30
	scoreContextFits    = 15
	scoreContextTight   = -40
	scoreVisionNeeded   = 40
	scoreVisionMissing  = -100
	scoreReasoning      = 12
	scoreLocalBonus     = 4
	scoreCooldown       = -60
	scoreUnhealthyEach  = 5
	scoreUnhealthyMax   = 20
	scoreDislikeEach    = -25
	scoreDislikeMax     = -75
	scoreCurrentCompany = 3
)

// scoreCandidate grades one (provider, model) pair against a usage profile.
// It is the single place where routing decisions are made, shared by the
// automatic routing in CandidatesFor and by /recommend.
func scoreCandidate(
	candidate Candidate,
	profile UsageProfile,
	state providerState,
) (float64, []string) {
	info := LookupModel(candidate.Model)
	score := 0.0

	var reasons []string

	needTokens := (float64(profile.HistoryMessages)*profile.AvgPromptChars +
		float64(profile.PeakHistoryChars)) / charsPerToken

	// Context window fit.
	switch {
	case needTokens <= 0:
		// No history yet: anything works.
	case float64(info.Context) >= needTokens*1.5:
		score += scoreContextRoomy
		reasons = append(reasons, "plenty of context headroom")
	case float64(info.Context) >= needTokens:
		score += scoreContextFits
		reasons = append(reasons, "fits your conversation length")
	default:
		score += scoreContextTight
		reasons = append(reasons, "context window is tight for your history")
	}

	// Vision is a hard requirement when it is used.
	if profile.UsesVision {
		if info.Vision {
			score += scoreVisionNeeded
			reasons = append(reasons, "handles images")
		} else {
			score += scoreVisionMissing
		}
	}

	// Volume: chatty users want throughput.
	speed := Speed(candidate.Provider)

	switch {
	case profile.RequestsPerDay > 50 || profile.PrefersFast:
		score += speed * 30
		if info.SizeB > 0 && info.SizeB <= 30 {
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
	if info.Reasoning && (profile.AvgPromptChars > 800 || profile.NeedsReasoning) {
		score += scoreReasoning
		reasons = append(reasons, "reasoning model suits long prompts")
	}

	// Local backends are free and unmetered: a good default for a public bot.
	if state.local {
		score += scoreLocalBonus
	}

	// Prefer the provider that is already in use so the answer does not churn.
	if state.active {
		score += scoreCurrentCompany
	}

	// Avoid backends that are currently failing.
	if state.cooldown {
		score += scoreCooldown
	}
	if profile.FailedRecently > 0 {
		penalty := math.Min(float64(profile.FailedRecently)*scoreUnhealthyEach, scoreUnhealthyEach*4)
		score -= penalty
	}

	// Respect explicit negative feedback.
	if dislikes := profile.DislikedModels[candidate.Model]; dislikes > 0 {
		penalty := math.Max(float64(dislikes)*scoreDislikeEach, scoreDislikeMax)
		score += penalty
		reasons = append(reasons, "you downvoted this model before")
	}

	return score, reasons
}

// providerState is the chain level context a score depends on.
type providerState struct {
	cooldown bool
	active   bool
	local    bool
}

// rank sorts candidates best first for a profile. It is stable, so models of
// equal score keep their configured order.
func rank(
	candidates []Candidate,
	profile UsageProfile,
	chain *Chain,
	now time.Time,
) []Candidate {
	if len(candidates) == 0 {
		return candidates
	}

	states := make(map[string]providerState, len(candidates))
	if chain != nil {
		chain.mu.RLock()
		active := ""
		if len(chain.providers) > 0 {
			active = chain.providers[chain.active].Name()
		}
		for _, p := range chain.providers {
			states[p.Name()] = providerState{
				cooldown: chain.inCooldown(p.Name(), now),
				active:   strings.EqualFold(p.Name(), active),
				local:    p.Keyless(),
			}
		}
		chain.mu.RUnlock()
	}

	scored := make([]struct {
		candidate Candidate
		score     float64
	}, len(candidates))

	for i, candidate := range candidates {
		score, _ := scoreCandidate(candidate, profile, states[candidate.Provider])
		scored[i].candidate = candidate
		scored[i].score = score
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	out := make([]Candidate, len(scored))
	for i, entry := range scored {
		out[i] = entry.candidate
	}

	return out
}

// Recommend scores every available (provider, model) pair against the usage
// profile and returns the best one with a human readable justification.
func (c *Chain) Recommend(profile UsageProfile) (Recommendation, error) {
	candidates := c.CandidatesFor(Preference{Auto: true}, &profile)
	if len(candidates) == 0 {
		return Recommendation{}, fmt.Errorf("no usable providers configured")
	}

	return c.RecommendAmong(candidates, profile), nil
}

// RecommendAmong explains which of the given candidates fits best. It is used
// by the answer footer, which already knows the shortlist.
func (c *Chain) RecommendAmong(candidates []Candidate, profile UsageProfile) Recommendation {
	if len(candidates) == 0 {
		return Recommendation{}
	}

	now := time.Now()

	best := candidates[0]
	bestScore := math.Inf(-1)
	var bestReasons []string

	for _, candidate := range candidates {
		var state providerState

		c.mu.RLock()
		active := ""
		if len(c.providers) > 0 {
			active = c.providers[c.active].Name()
		}
		state = providerState{
			cooldown: c.inCooldown(candidate.Provider, now),
			active:   strings.EqualFold(candidate.Provider, active),
		}
		if p := c.providerNamed(candidate.Provider); p != nil {
			state.local = p.Keyless()
		}
		c.mu.RUnlock()

		score, reasons := scoreCandidate(candidate, profile, state)
		if score > bestScore {
			bestScore = score
			best = candidate
			bestReasons = reasons
		}
	}

	reason := strings.Join(bestReasons, ", ")
	if reason == "" {
		reason = "best overall match for your current usage"
	}

	return Recommendation{
		Provider: best.Provider,
		Model:    best.Model,
		Reason:   reason,
		Score:    bestScore,
	}
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
