package agent

import (
	"fmt"
	"strings"
)

// Decision is the planner's parsed next step.
type Decision struct {
	Thought string
	Tool    string
	Input   string
	Final   string
	IsFinal bool
}

// BuildPlannerPrompt constructs the system and user prompt for the agent step.
func BuildPlannerPrompt(toolCatalog string, state *State) (string, string) {
	sys := "You are an autonomous AI agent that solves tasks step by step.\n" +
		toolCatalog + "\n\n" +
		"At each step, respond using EXACTLY one of these two formats:\n\n" +
		"Format 1 (when you need a tool):\n" +
		"THOUGHT: <brief reasoning>\n" +
		"ACTION: <tool_name>\n" +
		"INPUT: <tool_input>\n\n" +
		"Format 2 (when you have enough information to answer the user):\n" +
		"FINAL: <clear, helpful plain-text final answer>\n\n" +
		"Never call the same tool with identical input twice."

	var user strings.Builder
	user.WriteString("User Task: " + state.Task + "\n\n")
	if len(state.Steps) > 0 {
		user.WriteString("Previous steps:\n")
		for _, s := range state.Steps {
			user.WriteString(fmt.Sprintf(
				"Step %d:\nTHOUGHT: %s\nACTION: %s\nINPUT: %s\nOBSERVATION: %s\n\n",
				s.Step, s.Thought, s.Tool, s.Input, s.Observation,
			))
		}
	}
	user.WriteString("Decide the next step (ACTION or FINAL):")

	return sys, user.String()
}

// ParseDecision extracts either a tool call or a final answer from the model's
// completion text. If the model answered directly without tags, it is treated
// as a final answer so the user always gets a clean response.
func ParseDecision(raw string) Decision {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Decision{IsFinal: true, Final: ""}
	}

	if idx := strings.Index(raw, "FINAL:"); idx >= 0 {
		finalText := strings.TrimSpace(raw[idx+len("FINAL:"):])
		return Decision{
			IsFinal: true,
			Final:   finalText,
		}
	}

	var thought, action, input string
	lines := strings.Split(raw, "\n")
	inInput := false
	var inputLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "THOUGHT:"):
			inInput = false
			thought = strings.TrimSpace(trimmed[len("THOUGHT:"):])
		case strings.HasPrefix(upper, "ACTION:"):
			inInput = false
			action = strings.ToLower(strings.TrimSpace(trimmed[len("ACTION:"):]))
		case strings.HasPrefix(upper, "INPUT:"):
			inInput = true
			first := strings.TrimSpace(trimmed[len("INPUT:"):])
			if first != "" {
				inputLines = append(inputLines, first)
			}
		default:
			if inInput {
				inputLines = append(inputLines, line)
			}
		}
	}

	input = strings.TrimSpace(strings.Join(inputLines, "\n"))
	if action != "" && action != "none" {
		return Decision{
			Thought: thought,
			Tool:    action,
			Input:   input,
			IsFinal: false,
		}
	}

	return Decision{
		IsFinal: true,
		Final:   raw,
	}
}
