package user

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"openrouter-bot/config"
)

func newTestTracker(t *testing.T) (*UsageTracker, string) {
	t.Helper()

	dir := t.TempDir()
	tracker := NewUsageTracker("1", "tester", dir, &config.Config{SystemPrompt: "you are a test"}, nil)

	return tracker, dir
}

// TestDefaultsForNewUser pins the out of the box experience: automatic model
// choice and the model footer, so a new user gets the best model and can see
// which one answered.
func TestDefaultsForNewUser(t *testing.T) {
	tracker, _ := newTestTracker(t)

	settings := tracker.Settings()
	if !settings.Auto {
		t.Error("a new user should start in automatic mode")
	}
	if !settings.ShowFooter {
		t.Error("a new user should see which model answered")
	}

	preference := tracker.Preference()
	if !preference.Auto || preference.Model != "" {
		t.Errorf("Preference() = %#v, want auto", preference)
	}
}

// TestLegacyFileGetsDefaults covers an upgrade: an existing usage file has no
// settings block at all.
func TestLegacyFileGetsDefaults(t *testing.T) {
	dir := t.TempDir()

	legacy := []byte(`{"user_name":"old","usage_history":{"chat_cost":{"2024-01-01":0.5},"requests":{"2024-01-01":2}}}`)
	if err := os.WriteFile(filepath.Join(dir, "7.json"), legacy, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tracker := NewUsageTracker("7", "old", dir, nil, nil)

	settings := tracker.Settings()
	if !settings.Auto || !settings.ShowFooter {
		t.Errorf("legacy defaults not applied: %#v", settings)
	}

	// The spend history must survive the upgrade.
	if got := tracker.GetCurrentCost("total"); got != 0.5 {
		t.Errorf("total cost = %v, want 0.5", got)
	}
}

func TestPinnedModelAndProviderPersist(t *testing.T) {
	tracker, dir := newTestTracker(t)

	tracker.SetModel("llama-3.3-70b-versatile", "groq")

	preference := tracker.Preference()
	if preference.Auto || preference.Model != "llama-3.3-70b-versatile" || preference.Provider != "groq" {
		t.Fatalf("Preference() = %#v", preference)
	}

	// A second tracker on the same directory must see the same choice.
	reloaded := NewUsageTracker("1", "tester", dir, nil, nil)
	again := reloaded.Preference()
	if again.Model != preference.Model || again.Provider != preference.Provider {
		t.Errorf("reloaded preference = %#v, want %#v", again, preference)
	}

	// Pinning a provider keeps auto model choice inside it.
	tracker.SetProvider("gemini")
	preference = tracker.Preference()
	if preference.Provider != "gemini" || preference.Model != "" {
		t.Errorf("after SetProvider Preference() = %#v", preference)
	}

	// And auto clears both pins.
	tracker.ResetPreference()
	preference = tracker.Preference()
	if !preference.Auto || preference.Provider != "" || preference.Model != "" {
		t.Errorf("after ResetPreference Preference() = %#v", preference)
	}
}

func TestFeedbackSteersRouting(t *testing.T) {
	tracker, _ := newTestTracker(t)

	tracker.Feedback("some/model", false)
	tracker.Feedback("some/model", false)

	if got := tracker.DislikedModels()["some/model"]; got != 2 {
		t.Fatalf("downvotes = %d, want 2", got)
	}

	// One thumbs up removes one downvote, so a model can earn its way back.
	tracker.Feedback("some/model", true)
	if got := tracker.DislikedModels()["some/model"]; got != 1 {
		t.Errorf("after an upvote downvotes = %d, want 1", got)
	}
}

func TestFavouritesToggle(t *testing.T) {
	tracker, _ := newTestTracker(t)

	if !tracker.ToggleFavourite("gemini-2.5-flash") {
		t.Error("first toggle should add")
	}
	if !tracker.IsFavourite("gemini-2.5-flash") {
		t.Error("the model should be a favourite")
	}
	if tracker.ToggleFavourite("gemini-2.5-flash") {
		t.Error("second toggle should remove")
	}
	if tracker.IsFavourite("gemini-2.5-flash") {
		t.Error("the model should no longer be a favourite")
	}
}

func TestSettingsWrittenAsJSON(t *testing.T) {
	tracker, dir := newTestTracker(t)

	tracker.SetStreaming(false)

	data, err := os.ReadFile(filepath.Join(dir, "1.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var decoded struct {
		Settings Settings `json:"settings"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Settings.Streaming {
		t.Error("streaming should have been persisted as false")
	}
	if !decoded.Settings.Initialised {
		t.Error("settings should be marked initialised once written")
	}
}

// TestConversationHelpers covers the pieces "regenerate" relies on.
func TestConversationHelpers(t *testing.T) {
	tracker, _ := newTestTracker(t)

	tracker.AddMessage("user", "first question")
	tracker.AddMessage("assistant", "first answer")
	tracker.AddMessage("user", "second question")
	tracker.AddMessage("assistant", "second answer")

	if prompt, ok := tracker.LastUserMessage(); !ok || prompt != "second question" {
		t.Fatalf("LastUserMessage() = %q, %v", prompt, ok)
	}

	if !tracker.DropLastAssistant() {
		t.Fatal("DropLastAssistant should report a removal")
	}
	if !tracker.DropLastUser() {
		t.Fatal("DropLastUser should report a removal")
	}

	if prompt, _ := tracker.LastUserMessage(); prompt != "first question" {
		t.Errorf("after dropping, LastUserMessage() = %q", prompt)
	}
}
