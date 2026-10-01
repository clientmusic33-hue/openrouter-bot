// Package router provides task-aware, latency-aware smart model routing and
// concurrent model racing on top of the provider failover chain.
package router

import (
	"math"
	"sort"
	"strings"
	"time"

	"openrouter-bot/provider"
)

// TaskType identifies the category of a user request.
type TaskType string

const (
	TaskChat          TaskType = "CHAT"
	TaskCoding        TaskType = "CODING"
	TaskReasoning     TaskType = "REASONING"
	TaskVision        TaskType = "VISION"
	TaskTranslation   TaskType = "TRANSLATION"
	TaskSummarization TaskType = "SUMMARIZATION"
	TaskResearch      TaskType = "RESEARCH"
	TaskFast          TaskType = "FAST"
	TaskLongContext   TaskType = "LONG_CONTEXT"
)

// TaskAnalysis summarises what a prompt requires from the model.
type TaskAnalysis struct {
	Task             TaskType
	Complexity       int // 1 = simple, 2 = moderate, 3 = complex
	EstimatedTokens  int
	RequiresVision   bool
	RequiresCoding   bool
	RequiresReason   bool
	PrefersFast      bool
	NeedsLongContext bool
}

// RouteRequest describes the context available when choosing a model order.
type RouteRequest struct {
	Prompt       string
	HasImages    bool
	ContextChars int
	TaskOverride TaskType
	Preference   provider.Preference
	Profile      provider.UsageProfile
	PreferCheap  bool
}

var (
	codingKeywords = []string{
		"```", "func ", "function ", "def ", "class ", "import ", "package ",
		"interface ", "struct ", "select ", "from ", "dockerfile", "docker-compose",
		"stack trace", "traceback", "panic:", "segfault", "nullpointer", "typeerror",
		"syntaxerror", "compile", "refactor", "unit test", "bug", "regex", "sql",
		"golang", "python", "javascript", "typescript", "rust", "c++", "kubernetes",
	}

	reasoningKeywords = []string{
		"prove", "proof", "step by step", "step-by-step", "analyze deeply",
		"compare and contrast", "trade-offs", "tradeoffs", "architecture",
		"mathematical", "derive", "theorem", "equation", "integral", "differential",
		"logic puzzle", "why does", "explain in detail", "root cause",
	}

	translationKeywords = []string{
		"translate ", "translation", "in spanish", "in french", "in german",
		"in russian", "in hindi", "in chinese", "in japanese", "into english",
	}

	summarizationKeywords = []string{
		"summarize", "summary", "tl;dr", "tldr", "key takeaways",
		"action items", "bullet points", "condense", "recap",
	}

	researchKeywords = []string{
		"research ", "latest news", "current state of", "sources for",
		"literature review", "fact check", "investigate",
	}
)

// ClassifyTask inspects the prompt and request metadata to determine the
// primary task category and capability requirements.
func ClassifyTask(prompt string, hasImages bool, contextChars int) TaskAnalysis {
	trimmed := strings.TrimSpace(prompt)
	lower := strings.ToLower(trimmed)
	totalChars := len(trimmed) + contextChars
	estTokens := int(math.Ceil(float64(totalChars) / 3.5))

	analysis := TaskAnalysis{
		Task:            TaskChat,
		Complexity:      1,
		EstimatedTokens: estTokens,
		RequiresVision:  hasImages,
	}

	if hasImages {
		analysis.Task = TaskVision
		analysis.Complexity = 2
		return analysis
	}

	if estTokens > 16000 || totalChars > 50000 {
		analysis.NeedsLongContext = true
		analysis.Task = TaskLongContext
		analysis.Complexity = 3
	}

	switch {
	case containsAny(lower, codingKeywords):
		analysis.RequiresCoding = true
		if analysis.Task == TaskChat {
			analysis.Task = TaskCoding
		}
		analysis.Complexity = maxInt(analysis.Complexity, 2)
	case containsAny(lower, translationKeywords):
		if analysis.Task == TaskChat {
			analysis.Task = TaskTranslation
		}
		analysis.PrefersFast = true
	case containsAny(lower, summarizationKeywords):
		if analysis.Task == TaskChat {
			analysis.Task = TaskSummarization
		}
		analysis.Complexity = maxInt(analysis.Complexity, 2)
	case containsAny(lower, researchKeywords):
		if analysis.Task == TaskChat {
			analysis.Task = TaskResearch
		}
		analysis.RequiresReason = true
		analysis.Complexity = 3
	}

	if containsAny(lower, reasoningKeywords) || len(trimmed) > 900 {
		analysis.RequiresReason = true
		analysis.Complexity = 3
		if analysis.Task == TaskChat {
			analysis.Task = TaskReasoning
		}
	}

	// Short conversational messages benefit most from ultra-low latency.
	if analysis.Task == TaskChat && len(trimmed) > 0 && len(trimmed) <= 100 && !analysis.RequiresReason && !analysis.RequiresCoding {
		analysis.Task = TaskFast
		analysis.PrefersFast = true
		analysis.Complexity = 1
	}

	return analysis
}

// Route returns the ordered list of candidates best suited for req, falling
// back cleanly to the configured provider chain.
func Route(chain *provider.Chain, req RouteRequest) ([]provider.Candidate, TaskAnalysis) {
	analysis := ClassifyTask(req.Prompt, req.HasImages, req.ContextChars)
	if req.TaskOverride != "" {
		analysis.Task = req.TaskOverride
		switch req.TaskOverride {
		case TaskFast:
			analysis.PrefersFast = true
		case TaskCoding:
			analysis.RequiresCoding = true
		case TaskReasoning:
			analysis.RequiresReason = true
		case TaskVision:
			analysis.RequiresVision = true
		case TaskLongContext:
			analysis.NeedsLongContext = true
		}
	}

	if chain == nil {
		return nil, analysis
	}

	profile := req.Profile
	if analysis.RequiresVision {
		profile.UsesVision = true
	}
	if analysis.RequiresCoding {
		profile.NeedsCoding = true
	}
	if analysis.RequiresReason {
		profile.NeedsReasoning = true
	}
	if analysis.PrefersFast {
		profile.PrefersFast = true
	}
	profile.Task = string(analysis.Task)

	base := chain.CandidatesFor(req.Preference, &profile)
	if len(base) <= 1 {
		return base, analysis
	}

	// Respect an explicit user model pin when that model's provider is healthy.
	if !req.Preference.Auto && strings.TrimSpace(req.Preference.Model) != "" {
		return base, analysis
	}

	// If neither auto nor a specific task override is active, keep chain order.
	if !req.Preference.Auto && req.TaskOverride == "" && req.Profile.RequestsPerDay == 0 && !req.HasImages {
		return base, analysis
	}

	providerInfos := make(map[string]provider.Info)
	for _, info := range chain.Status() {
		providerInfos[strings.ToLower(info.Name)] = info
	}

	type scoredCandidate struct {
		cand  provider.Candidate
		score float64
	}

	scored := make([]scoredCandidate, len(base))
	for i, cand := range base {
		pInfo := providerInfos[strings.ToLower(cand.Provider)]
		modelLat, _ := chain.ModelLatency(cand.Model)
		score := scoreForTask(cand, analysis, profile, pInfo, modelLat, req.PreferCheap, i)
		scored[i] = scoredCandidate{cand: cand, score: score}
	}

	// If a provider is pinned, only sort within that provider first, keeping
	// fallback providers after it.
	pinnedProvider := strings.TrimSpace(req.Preference.Provider)
	sort.SliceStable(scored, func(i, j int) bool {
		if pinnedProvider != "" {
			iPinned := strings.EqualFold(scored[i].cand.Provider, pinnedProvider)
			jPinned := strings.EqualFold(scored[j].cand.Provider, pinnedProvider)
			if iPinned != jPinned {
				return iPinned
			}
		}
		return scored[i].score > scored[j].score
	})

	out := make([]provider.Candidate, len(scored))
	for i, item := range scored {
		out[i] = item.cand
	}

	return out, analysis
}

func scoreForTask(
	cand provider.Candidate,
	analysis TaskAnalysis,
	profile provider.UsageProfile,
	pInfo provider.Info,
	modelLatency time.Duration,
	preferCheap bool,
	baseRank int,
) float64 {
	mInfo := provider.LookupModel(cand.Model)
	score := 100.0 - float64(baseRank)*1.5

	// Health & circuit breaker checks.
	if pInfo.Circuit == provider.CircuitOpen || pInfo.Cooldown > 0 {
		score -= 80
	} else if pInfo.Circuit == provider.CircuitHalfOpen || pInfo.Failures > 0 {
		score -= float64(pInfo.Failures) * 8
	}

	// Task-specific capability matching.
	switch analysis.Task {
	case TaskVision:
		if mInfo.Vision {
			score += 60
		} else {
			score -= 120
		}
	case TaskCoding:
		if mInfo.Coding {
			score += 35
		}
		if mInfo.Reasoning {
			score += 15
		}
		if mInfo.SizeB >= 30 {
			score += 10
		}
	case TaskReasoning, TaskResearch:
		if mInfo.Reasoning {
			score += 40
		}
		if mInfo.SizeB >= 70 {
			score += 20
		}
	case TaskFast, TaskTranslation:
		speed := provider.Speed(cand.Provider)
		score += speed * 45
		if mInfo.Fast {
			score += 25
		}
		if mInfo.Reasoning {
			// Chain-of-thought models have slower time-to-first-token for simple queries.
			score -= 15
		}
	case TaskLongContext, TaskSummarization:
		if mInfo.Context >= 128000 {
			score += 35
		} else if mInfo.Context < analysis.EstimatedTokens {
			score -= 60
		}
	default:
		// General CHAT: balance quality and speed.
		score += provider.Speed(cand.Provider) * 20
		if mInfo.SizeB >= 70 {
			score += 15
		}
	}

	// Context window check regardless of task.
	if analysis.EstimatedTokens > 0 && mInfo.Context > 0 {
		if mInfo.Context < analysis.EstimatedTokens {
			score -= 75
		} else if mInfo.Context >= analysis.EstimatedTokens*2 {
			score += 10
		}
	}

	// Measured latency adjustment.
	lat := modelLatency
	if lat == 0 {
		lat = pInfo.AvgLatency
	}
	if lat > 0 {
		switch {
		case lat < 600*time.Millisecond:
			score += 15
		case lat < 1500*time.Millisecond:
			score += 8
		case lat > 10*time.Second:
			score -= 18
		case lat > 5*time.Second:
			score -= 8
		}
	}

	// Cost preference.
	if preferCheap {
		score -= float64(mInfo.CostTier) * 12
	}

	// User negative feedback penalty.
	if dislikes := profile.DislikedModels[cand.Model]; dislikes > 0 {
		score -= math.Min(float64(dislikes)*30, 90)
	}

	return score
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}

	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}

	return b
}
