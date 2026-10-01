// Package personas provides modular AI assistant personas (Developer, Teacher,
// Researcher, Writer, Translator, Coding Agent, Business Assistant) and custom
// per-user persona definitions.
package personas

import (
	"sort"
	"strings"
)

// Persona describes one role/style preset for the assistant.
type Persona struct {
	Key         string
	Name        string
	Emoji       string
	Description string
	Prompt      string
}

var builtin = map[string]Persona{
	"default": {
		Key:         "default",
		Name:        "Default Assistant",
		Emoji:       "🤖",
		Description: "Friendly, concise general-purpose assistant",
		Prompt:      "",
	},
	"developer": {
		Key:         "developer",
		Name:        "Developer",
		Emoji:       "👨‍💻",
		Description: "Pragmatic senior software engineer focused on clean, production-grade code",
		Prompt:      "Act as a pragmatic senior software engineer. Focus on clean architecture, edge cases, performance, security, and concise idiomatic code.",
	},
	"teacher": {
		Key:         "teacher",
		Name:        "Teacher",
		Emoji:       "🎓",
		Description: "Patient educator who explains concepts step-by-step with clear analogies",
		Prompt:      "Act as a patient, encouraging teacher. Break complex ideas down step by step, use intuitive analogies, and verify understanding.",
	},
	"researcher": {
		Key:         "researcher",
		Name:        "Researcher",
		Emoji:       "🔬",
		Description: "Analytical researcher who prioritises evidence, nuance, and citations",
		Prompt:      "Act as a rigorous research analyst. Separate established facts from hypotheses, highlight trade-offs, and structure findings clearly.",
	},
	"writer": {
		Key:         "writer",
		Name:        "Writer",
		Emoji:       "✍️",
		Description: "Polished editor and creative writer with strong style and clarity",
		Prompt:      "Act as a skilled writer and editor. Prioritise vivid, clear, engaging prose, strong structure, and appropriate tone.",
	},
	"translator": {
		Key:         "translator",
		Name:        "Translator",
		Emoji:       "🌐",
		Description: "Fluent polyglot translator preserving tone, idioms, and formatting",
		Prompt:      "Act as a professional polyglot translator. Preserve nuances, cultural idioms, emojis, and formatting accurately.",
	},
	"coding_agent": {
		Key:         "coding_agent",
		Name:        "Coding Agent",
		Emoji:       "⚡",
		Description: "Autonomous debugging, refactoring, and code-review specialist",
		Prompt:      "Act as an autonomous coding agent. Diagnose root causes, propose minimal targeted patches, point out bugs or race conditions, and suggest tests.",
	},
	"business": {
		Key:         "business",
		Name:        "Business Assistant",
		Emoji:       "💼",
		Description: "Executive advisor for strategy, emails, summaries, and action items",
		Prompt:      "Act as an executive business assistant. Be structured, action-oriented, and concise. Highlight key decisions, risks, and action items.",
	},
}

var aliases = map[string]string{
	"dev":                "developer",
	"code":               "coding_agent",
	"coder":              "coding_agent",
	"coding":             "coding_agent",
	"coding agent":       "coding_agent",
	"coding-agent":       "coding_agent",
	"business assistant": "business",
	"business-assistant": "business",
	"biz":                "business",
	"auto":               "default",
	"off":                "default",
	"reset":              "default",
}

// List returns all built-in personas in a deterministic order.
func List() []Persona {
	order := []string{
		"default",
		"developer",
		"coding_agent",
		"teacher",
		"researcher",
		"writer",
		"translator",
		"business",
	}

	out := make([]Persona, 0, len(order))
	for _, k := range order {
		if p, ok := builtin[k]; ok {
			out = append(out, p)
		}
	}

	// Include any extra keys if added later.
	if len(out) < len(builtin) {
		var extra []string
		for k := range builtin {
			found := false
			for _, o := range order {
				if k == o {
					found = true
					break
				}
			}
			if !found {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		for _, k := range extra {
			out = append(out, builtin[k])
		}
	}

	return out
}

// Lookup resolves a persona by key, name, or alias.
func Lookup(name string) (Persona, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	if mapped, ok := aliases[key]; ok {
		key = mapped
	}
	if p, ok := builtin[key]; ok {
		return p, true
	}
	for _, p := range builtin {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Persona{}, false
}
