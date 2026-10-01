package config

import (
	"strings"
	"testing"
)

// TestValidateFillsNewDefaults covers the knobs added with the button-based
// UI: a config file written for an older version must still produce a working
// bot.
func TestValidateFillsNewDefaults(t *testing.T) {
	c := &Config{BudgetPeriod: "monthly"}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if c.MaxReplyChars != DefaultMaxReplyChars {
		t.Errorf("MaxReplyChars = %d, want %d", c.MaxReplyChars, DefaultMaxReplyChars)
	}
	if c.GroupAccess != AccessEveryone {
		t.Errorf("GroupAccess = %q, want %q", c.GroupAccess, AccessEveryone)
	}
	if strings.TrimSpace(c.PersonaPrompt) == "" {
		t.Error("the default persona must be applied")
	}
}

// TestValidateClampsReplyChars is the guard against a config that would make
// Telegram reject every long answer.
func TestValidateClampsReplyChars(t *testing.T) {
	c := &Config{BudgetPeriod: "monthly", MaxReplyChars: 99999}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if c.MaxReplyChars > TelegramMaxMessageLength-96 {
		t.Errorf("MaxReplyChars = %d, want it clamped below the Telegram limit", c.MaxReplyChars)
	}
}

func TestValidateKeepsValidGroupAccess(t *testing.T) {
	for _, mode := range []string{AccessEveryone, AccessAdmins, AccessOwner} {
		c := &Config{BudgetPeriod: "monthly", GroupAccess: mode}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate(%s): %v", mode, err)
		}
		if c.GroupAccess != mode {
			t.Errorf("GroupAccess = %q, want %q", c.GroupAccess, mode)
		}
	}
}

// TestSystemPromptCarriesPersona pins the response contract: structured
// answers in Telegram-safe Markdown, never HTML or LaTeX, and short answers.
// Telegram rejects a message whose markup does not parse, so the persona has to
// ask only for markup the renderer can fall back from - that is the reliability
// feature, not the plain-text rule it used to be.
func TestSystemPromptCarriesPersona(t *testing.T) {
	c := &Config{
		Lang:          "EN",
		SystemPrompt:  "You are a test assistant.",
		PersonaPrompt: DefaultPersonaPrompt,
		MaxReplyChars: DefaultMaxReplyChars,
	}

	prompt := buildSystemPrompt(c)

	for _, want := range []string{
		"Always answer in English language.",
		"You are a test assistant.",
		"Format for Telegram Markdown",
		"Never emit HTML",
		"Explain in plain words first",
		"Write maths the way a phone shows it",
		"Never emit LaTeX",
		"One short line per step",
		"Never write more than 3500 characters",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt is missing %q:\n%s", want, prompt)
		}
	}
}

func TestIsAdmin(t *testing.T) {
	c := &Config{AdminChatIDs: []int64{11, 22}}

	if !c.IsAdmin(22) {
		t.Error("22 should be an owner")
	}
	if c.IsAdmin(33) {
		t.Error("33 is not in the owner list")
	}
	if (&Config{}).IsAdmin(1) {
		t.Error("an empty owner list must match nobody")
	}
}
