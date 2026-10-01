package memory

import (
	"fmt"
	"strings"
	"sync"

	"openrouter-bot/user"
)

// ContextConfig controls token optimization and conversation summarization.
type ContextConfig struct {
	MaxContextMessages int
	MaxContextTokens   int
	SummaryThreshold   int
}

// DefaultContextConfig returns sensible token-saving defaults.
func DefaultContextConfig() ContextConfig {
	return ContextConfig{
		MaxContextMessages: 8,
		MaxContextTokens:   3000,
		SummaryThreshold:   10,
	}
}

type summaryState struct {
	summarizedCount int
	summaryText     string
}

// Optimizer compacts conversation history so only recent messages, a compact
// summary of older turns, and relevant long-term memory are sent upstream.
type Optimizer struct {
	mu        sync.Mutex
	summaries map[string]summaryState
}

// NewOptimizer creates a context optimizer.
func NewOptimizer() *Optimizer {
	return &Optimizer{
		summaries: make(map[string]summaryState),
	}
}

// EstimateTokens approximates the token count of text.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

// Optimize takes the full history for a conversation and returns:
//  1. the pruned slice of recent messages that fit within MaxContextMessages & MaxContextTokens
//  2. a compact summary of older messages (only computed when history reaches SummaryThreshold)
func (o *Optimizer) Optimize(convKey string, history []user.Message, cfg ContextConfig) (recent []user.Message, summary string) {
	if cfg.MaxContextMessages <= 0 {
		cfg.MaxContextMessages = 8
	}
	if cfg.MaxContextTokens <= 0 {
		cfg.MaxContextTokens = 3000
	}
	if cfg.SummaryThreshold <= 0 {
		cfg.SummaryThreshold = 10
	}

	if len(history) == 0 {
		return nil, ""
	}

	// Only summarize when history reaches SummaryThreshold, and cache the
	// summary until at least 4 more messages accumulate.
	if len(history) >= cfg.SummaryThreshold && len(history) > cfg.MaxContextMessages {
		olderCount := len(history) - cfg.MaxContextMessages
		older := history[:olderCount]
		recent = history[olderCount:]

		if o != nil && convKey != "" {
			o.mu.Lock()
			st, ok := o.summaries[convKey]
			if !ok || olderCount-st.summarizedCount >= 4 || st.summaryText == "" {
				st = summaryState{
					summarizedCount: olderCount,
					summaryText:     compactSummarize(older),
				}
				o.summaries[convKey] = st
			}
			summary = st.summaryText
			o.mu.Unlock()
		} else {
			summary = compactSummarize(older)
		}
	} else {
		if len(history) > cfg.MaxContextMessages {
			recent = history[len(history)-cfg.MaxContextMessages:]
		} else {
			recent = history
		}
	}

	// Enforce MaxContextTokens on recent messages from newest backwards.
	budget := cfg.MaxContextTokens - EstimateTokens(summary)
	if budget < 256 {
		budget = 256
	}

	used := 0
	cutIdx := len(recent)
	for i := len(recent) - 1; i >= 0; i-- {
		msgTokens := EstimateTokens(recent[i].Content) + 4
		if used+msgTokens > budget && i < len(recent)-1 {
			break
		}
		used += msgTokens
		cutIdx = i
	}

	return recent[cutIdx:], summary
}

// compactSummarize builds a deterministic, zero-API-call extractive summary of
// older conversation turns so summarization adds zero latency and zero token cost.
func compactSummarize(messages []user.Message) string {
	if len(messages) == 0 {
		return ""
	}

	var points []string
	for _, m := range messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		firstLine := strings.SplitN(content, "\n", 2)[0]
		runes := []rune(firstLine)
		if len(runes) > 110 {
			firstLine = string(runes[:110]) + "…"
		}
		role := "User"
		if m.Role == "assistant" {
			role = "Assistant"
		}
		points = append(points, fmt.Sprintf("- %s: %s", role, firstLine))
	}

	if len(points) > 6 {
		points = points[len(points)-6:]
	}

	return "Earlier conversation summary:\n" + strings.Join(points, "\n")
}
