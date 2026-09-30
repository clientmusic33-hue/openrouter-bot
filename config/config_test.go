package config

import (
	"testing"
	"time"
)

func TestValidateAppliesDefaults(t *testing.T) {
	c := &Config{
		BudgetPeriod: "MONTHLY",
		Model:        ModelParameters{ModelName: "test/model"},
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if c.Lang != "EN" {
		t.Errorf("Lang = %q, want EN", c.Lang)
	}
	if c.BudgetPeriod != "monthly" {
		t.Errorf("BudgetPeriod = %q, want monthly", c.BudgetPeriod)
	}
	if c.GroupChatMode != GroupModeMention {
		t.Errorf("GroupChatMode = %q, want %q", c.GroupChatMode, GroupModeMention)
	}
	if c.RequestTimeout != 300*time.Second {
		t.Errorf("RequestTimeout = %v, want 300s", c.RequestTimeout)
	}
	if c.MaxConcurrentRequests != 8 {
		t.Errorf("MaxConcurrentRequests = %d, want 8", c.MaxConcurrentRequests)
	}
}

func TestValidateRejectsBadBudgetPeriod(t *testing.T) {
	c := &Config{BudgetPeriod: "hourly"}

	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject an unknown budget period")
	}
}

func TestValidateRejectsEmptyBudgetPeriod(t *testing.T) {
	c := &Config{BudgetPeriod: ""}

	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject an empty budget period")
	}
}

func TestCloneIsDeep(t *testing.T) {
	c := &Config{
		Lang:               "EN",
		AdminChatIDs:       []int64{1, 2},
		Model:              ModelParameters{ModelName: "a/b"},
		RequestTimeout:     5 * time.Second,
		MaxHistorySize:     10,
		MaxHistoryTime:     60,
		BudgetPeriod:       "monthly",
		GroupChatMode:      GroupModeMention,
		StatsMinRole:       "ADMIN",
		AllowedUserChatIDs: []int64{7},
	}

	clone := c.Clone()

	clone.AdminChatIDs[0] = 99
	clone.Model.ModelName = "changed"
	clone.AllowedUserChatIDs[0] = 42

	if c.AdminChatIDs[0] != 1 {
		t.Error("Clone must copy the admin id slice")
	}
	if c.AllowedUserChatIDs[0] != 7 {
		t.Error("Clone must copy the allowed user id slice")
	}
	if c.Model.ModelName != "a/b" {
		t.Error("Clone must not share nested structs")
	}
}

func TestCloneNil(t *testing.T) {
	if got := (*Config)(nil).Clone(); got != nil {
		t.Errorf("Clone of nil should be nil, got %#v", got)
	}
}

func TestMaskHidesSecrets(t *testing.T) {
	cases := map[string]bool{
		"":                    false,
		"short":               false,
		"1234567890":          true,
		"sk-or-v1-abcdefghij": true,
	}

	for input, shouldHide := range cases {
		got := mask(input)
		if shouldHide && got == input {
			t.Errorf("mask(%q) leaked the secret: %q", input, got)
		}
		if !shouldHide && input == "" && got != "<unset>" {
			t.Errorf("mask(\"\") = %q, want <unset>", got)
		}
	}

	if got := mask("sk-or-v1-abcdefghij"); got[:4] != "sk-o" {
		t.Errorf("mask should keep a short prefix, got %q", got)
	}
}

func TestLanguageName(t *testing.T) {
	if got := languageName("RU"); got != "Russian" {
		t.Errorf("languageName(RU) = %q", got)
	}
	if got := languageName("en"); got != "English" {
		t.Errorf("languageName(en) = %q", got)
	}
	if got := languageName("FR"); got != "FR" {
		t.Errorf("languageName(FR) = %q, want FR", got)
	}
}
