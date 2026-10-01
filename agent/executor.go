package agent

import (
	"context"
	"fmt"
	"strings"

	"openrouter-bot/tools"
)

// Executor runs a planned tool action while enforcing permissions, timeouts,
// and duplicate-call loop prevention.
type Executor struct {
	registry *tools.Registry
}

// NewExecutor wraps a tool registry.
func NewExecutor(reg *tools.Registry) *Executor {
	return &Executor{registry: reg}
}

// Execute runs decision against the tool registry and returns the observation.
func (e *Executor) Execute(ctx context.Context, cfg Config, state *State, d Decision) StepRecord {
	rec := StepRecord{
		Step:    len(state.Steps) + 1,
		Thought: d.Thought,
		Tool:    d.Tool,
		Input:   d.Input,
	}

	if e == nil || e.registry == nil {
		rec.Error = "no tool registry configured"
		rec.Observation = "Error: no tools are available."
		return rec
	}

	// Loop guard: prevent repeating the exact same (tool, input) call.
	for _, prev := range state.Steps {
		if strings.EqualFold(prev.Tool, d.Tool) && strings.TrimSpace(prev.Input) == strings.TrimSpace(d.Input) {
			rec.Error = "duplicate tool invocation blocked"
			rec.Observation = fmt.Sprintf("Tool %q was already called with input %q; use the previous observation and provide FINAL.", d.Tool, d.Input)
			return rec
		}
	}

	out, err := e.registry.Run(ctx, d.Tool, cfg.UserID, cfg.Role, d.Input)
	if err != nil {
		rec.Error = err.Error()
		rec.Observation = "Tool error: " + err.Error()
		return rec
	}

	if len(out) > 2500 {
		out = out[:2500] + "…"
	}
	rec.Observation = out
	return rec
}
