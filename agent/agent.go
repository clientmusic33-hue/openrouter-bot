package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"openrouter-bot/provider"
	"openrouter-bot/router"
	"openrouter-bot/tools"

	"github.com/sashabaranov/go-openai"
)

// Agent coordinates planning, tool execution, observation, and synthesis.
type Agent struct {
	chain    *provider.Chain
	executor *Executor
	registry *tools.Registry
}

// New creates a new Agent backed by chain and reg.
func New(chain *provider.Chain, reg *tools.Registry) *Agent {
	return &Agent{
		chain:    chain,
		registry: reg,
		executor: NewExecutor(reg),
	}
}

// Run executes the agent loop up to cfg.MaxSteps and cfg.Timeout.
func (a *Agent) Run(parentCtx context.Context, task string, cfg Config) (*State, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return nil, errors.New("agent task must not be empty")
	}
	if a == nil || a.chain == nil {
		return nil, errors.New("agent provider chain is not configured")
	}

	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}
	if maxSteps > 10 {
		maxSteps = 10
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	if cfg.Role == "" {
		cfg.Role = "USER"
	}

	state := &State{Task: task}
	toolCatalog := FormatToolCatalog(a.registry, cfg.Role)

	candidates, _ := router.Route(a.chain, router.RouteRequest{
		Prompt:       task,
		TaskOverride: router.TaskReasoning,
		Preference:   provider.Preference{Auto: true},
	})
	if len(candidates) == 0 {
		return nil, errors.New("no usable models in the provider chain")
	}

	for step := 1; step <= maxSteps; step++ {
		if ctx.Err() != nil {
			break
		}

		sysPrompt, userPrompt := BuildPlannerPrompt(toolCatalog, state)
		reply, provName, modelName, err := a.completeStep(ctx, candidates, sysPrompt, userPrompt)
		if err != nil {
			if len(state.Steps) > 0 {
				break
			}
			return nil, err
		}
		state.Provider = provName
		state.Model = modelName

		decision := ParseDecision(reply)
		if decision.IsFinal {
			state.FinalAnswer = decision.Final
			return state, nil
		}

		rec := a.executor.Execute(ctx, cfg, state, decision)
		state.Steps = append(state.Steps, rec)
	}

	// Reached maxSteps without a FINAL answer: ask the model to synthesise a
	// final answer from the observations gathered so far.
	sysPrompt := "Synthesize a clear, direct final answer to the user's task based on the observations gathered."
	_, userPrompt := BuildPlannerPrompt(toolCatalog, state)
	userPrompt += "\n\nMaximum tool steps reached. Provide FINAL answer now:"

	if reply, provName, modelName, err := a.completeStep(ctx, candidates, sysPrompt, userPrompt); err == nil {
		state.Provider = provName
		state.Model = modelName
		d := ParseDecision(reply)
		if d.Final != "" {
			state.FinalAnswer = d.Final
			return state, nil
		}
	}

	if len(state.Steps) > 0 {
		last := state.Steps[len(state.Steps)-1]
		state.FinalAnswer = fmt.Sprintf("Completed %d step(s). Latest result: %s", len(state.Steps), last.Observation)
		return state, nil
	}

	return nil, errors.New("agent could not complete the task within the step limit")
}

func (a *Agent) completeStep(
	ctx context.Context,
	candidates []provider.Candidate,
	sysPrompt, userPrompt string,
) (string, string, string, error) {
	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: sysPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userPrompt},
		},
		Temperature: 0.2,
		MaxTokens:   1200,
	}

	var lastErr error
	for _, cand := range candidates {
		start := time.Now()
		resp, err := cand.Complete(ctx, req)
		if err != nil {
			lastErr = err
			a.chain.RecordFailure(cand.Provider, err)
			if ctx.Err() != nil {
				return "", "", "", ctx.Err()
			}
			continue
		}
		if len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
			lastErr = errors.New("empty agent step completion")
			a.chain.RecordFailure(cand.Provider, lastErr)
			continue
		}

		a.chain.RecordSuccessLatency(cand.Provider, cand.Model, time.Since(start))
		return strings.TrimSpace(resp.Choices[0].Message.Content), cand.Provider, cand.Model, nil
	}

	return "", "", "", fmt.Errorf("agent completion failed: %w", lastErr)
}
