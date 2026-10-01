// Package agent implements a modular tool-using AI agent with bounded steps,
// loop prevention, and strict execution timeouts.
package agent

import "time"

// DefaultMaxSteps bounds the reasoning-action loop when MAX_AGENT_STEPS is unset.
const DefaultMaxSteps = 5

// DefaultTimeout bounds the total agent run duration.
const DefaultTimeout = 90 * time.Second

// Config controls one agent execution.
type Config struct {
	MaxSteps int
	Timeout  time.Duration
	UserID   string
	Role     string
}

// StepRecord captures one thought -> tool call -> observation cycle.
type StepRecord struct {
	Step        int
	Thought     string
	Tool        string
	Input       string
	Observation string
	Error       string
}

// State holds the running trace of an agent task.
type State struct {
	Task        string
	Steps       []StepRecord
	FinalAnswer string
	Provider    string
	Model       string
}
