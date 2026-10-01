package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openrouter-bot/provider"
	"openrouter-bot/tools"
)

func TestAgentExecutesToolAndReturnsFinalAnswer(t *testing.T) {
	var turn atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := turn.Add(1)
		content := "FINAL: The result is 144."
		if n == 1 {
			content = "THOUGHT: I should compute 12 * 12 using calculator\nACTION: calculator\nINPUT: 12 * 12"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "step",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": content}},
			},
		})
	}))
	defer srv.Close()

	chain, err := provider.NewChain([]provider.Config{
		{Name: "local", BaseURL: srv.URL, Models: []string{"test-model"}},
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	reg := tools.DefaultRegistry(chain, nil)
	ag := New(chain, reg)

	state, err := ag.Run(context.Background(), "What is 12 * 12?", Config{
		MaxSteps: 4,
		Timeout:  10 * time.Second,
		UserID:   "u1",
		Role:     "USER",
	})
	if err != nil {
		t.Fatalf("Agent.Run: %v", err)
	}

	if len(state.Steps) != 1 || state.Steps[0].Observation != "144" {
		t.Fatalf("expected 1 calculator step with observation 144, got %+v", state.Steps)
	}
	if !strings.Contains(state.FinalAnswer, "144") {
		t.Errorf("FinalAnswer = %q, want 144", state.FinalAnswer)
	}
}

func TestAgentEnforcesStepLimitAndLoopPrevention(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Model tries to loop forever on the same tool call.
		content := "THOUGHT: keep calculating\nACTION: calculator\nINPUT: 2 + 2"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "loop",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": content}},
			},
		})
	}))
	defer srv.Close()

	chain, err := provider.NewChain([]provider.Config{
		{Name: "local", BaseURL: srv.URL, Models: []string{"test-model"}},
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	ag := New(chain, tools.DefaultRegistry(chain, nil))
	state, err := ag.Run(context.Background(), "loop test", Config{
		MaxSteps: 2,
		Timeout:  5 * time.Second,
		UserID:   "u1",
		Role:     "USER",
	})
	if err != nil {
		t.Fatalf("Agent.Run: %v", err)
	}
	if len(state.Steps) > 2 {
		t.Fatalf("agent exceeded MaxSteps=2: got %d steps", len(state.Steps))
	}
	if len(state.Steps) == 2 && state.Steps[1].Error == "" {
		t.Errorf("expected duplicate tool invocation to be flagged on step 2, got %+v", state.Steps[1])
	}
}
