package memory

import (
	"strings"
	"testing"

	jsonstore "openrouter-bot/storage/json"
	"openrouter-bot/user"
)

func TestMemoryLifecycleAndSensitivityFilter(t *testing.T) {
	mgr := NewManager(jsonstore.New(t.TempDir()))

	// Sensitive facts must be rejected.
	if _, err := mgr.Add("u1", "My API_KEY is sk-or-v1-1234567890abcdef"); err == nil {
		t.Fatal("expected sensitive fact to be rejected")
	}
	if _, err := mgr.Add("u1", "My password is hunter2"); err == nil {
		t.Fatal("expected password fact to be rejected")
	}

	f1, err := mgr.Add("u1", "Prefers Go 1.25 and concise code examples")
	if err != nil {
		t.Fatalf("Add valid fact: %v", err)
	}
	if f1.ID != 1 {
		t.Errorf("fact ID = %d, want 1", f1.ID)
	}

	// Implicit extraction.
	if _, ok := mgr.MaybeExtractImplicitFact("u1", "Remember that I deploy on Render using Docker"); !ok {
		t.Error("expected implicit fact to be extracted")
	}

	list := mgr.List("u1")
	if len(list) != 2 {
		t.Fatalf("List() len = %d, want 2", len(list))
	}

	ctxText := mgr.RelevantContext("u1", "How do I deploy my Go app?", 5)
	if !strings.Contains(ctxText, "Go 1.25") {
		t.Errorf("RelevantContext missing expected fact: %q", ctxText)
	}

	// Forget by ID.
	removed, err := mgr.Forget("u1", "1")
	if err != nil || removed != 1 {
		t.Fatalf("Forget(1) = %d, %v", removed, err)
	}

	// Disable memory.
	if err := mgr.SetEnabled("u1", false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if mgr.RelevantContext("u1", "anything", 5) != "" {
		t.Error("disabled memory must return empty context")
	}
}

func TestOptimizerSummarizesOnlyAboveThreshold(t *testing.T) {
	opt := NewOptimizer()
	cfg := ContextConfig{
		MaxContextMessages: 3,
		MaxContextTokens:   1000,
		SummaryThreshold:   5,
	}

	short := []user.Message{
		{Role: "user", Content: "m1"},
		{Role: "assistant", Content: "m2"},
		{Role: "user", Content: "m3"},
	}

	recent, summary := opt.Optimize("conv-1", short, cfg)
	if summary != "" {
		t.Errorf("expected no summary below threshold, got %q", summary)
	}
	if len(recent) != 3 {
		t.Errorf("len(recent) = %d, want 3", len(recent))
	}

	long := []user.Message{
		{Role: "user", Content: "first topic about databases"},
		{Role: "assistant", Content: "explained indexing"},
		{Role: "user", Content: "second topic about caching"},
		{Role: "assistant", Content: "explained redis"},
		{Role: "user", Content: "recent question 1"},
		{Role: "assistant", Content: "recent answer 1"},
	}

	recent, summary = opt.Optimize("conv-1", long, cfg)
	if summary == "" || !strings.Contains(summary, "databases") {
		t.Errorf("expected summary above threshold, got %q", summary)
	}
	if len(recent) != 3 {
		t.Errorf("len(recent) = %d, want 3", len(recent))
	}
}
