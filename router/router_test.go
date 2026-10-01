package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"openrouter-bot/provider"

	"github.com/sashabaranov/go-openai"
)

func TestClassifyTaskCategories(t *testing.T) {
	cases := []struct {
		name      string
		prompt    string
		hasImages bool
		ctxChars  int
		want      TaskType
	}{
		{"fast short greeting", "hi how are you?", false, 0, TaskFast},
		{"vision with photo", "what is in this photo?", true, 0, TaskVision},
		{"coding prompt", "Please refactor this func main() and fix the stack trace panic:", false, 0, TaskCoding},
		{"reasoning prompt", "Prove step by step why this mathematical theorem holds", false, 0, TaskReasoning},
		{"translation prompt", "Translate this paragraph into Spanish", false, 0, TaskTranslation},
		{"summarization prompt", "Give me a TL;DR summary and action items for this meeting", false, 0, TaskSummarization},
		{"research prompt", "Research the latest news and sources for fusion energy", false, 0, TaskResearch},
		{"long context", "Analyze this document", false, 70000, TaskLongContext},
	}

	for _, tc := range cases {
		got := ClassifyTask(tc.prompt, tc.hasImages, tc.ctxChars)
		if got.Task != tc.want {
			t.Errorf("%s: ClassifyTask() = %s, want %s", tc.name, got.Task, tc.want)
		}
	}
}

func TestRouteSelectsCapabilityMatchedModel(t *testing.T) {
	chain, err := provider.NewChain([]provider.Config{
		{
			Name:    "groq",
			BaseURL: "https://api.groq.com/openai/v1",
			APIKey:  "k1",
			Models:  []string{"llama-3.1-8b-instant", "llama-3.3-70b-versatile"},
		},
		{
			Name:    "gemini",
			BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/",
			APIKey:  "k2",
			Models:  []string{"gemini-2.5-flash"},
		},
		{
			Name:    "openrouter",
			BaseURL: "https://openrouter.ai/api/v1",
			APIKey:  "k3",
			Models:  []string{"qwen/qwen3-coder:free", "deepseek/deepseek-r1:free"},
		},
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	// Vision task should pick a vision-capable model (gemini-2.5-flash).
	visionCands, analysis := Route(chain, RouteRequest{
		Prompt:     "What is in this image?",
		HasImages:  true,
		Preference: provider.Preference{Auto: true},
	})
	if analysis.Task != TaskVision {
		t.Fatalf("expected VISION task, got %s", analysis.Task)
	}
	if len(visionCands) == 0 || !provider.LookupModel(visionCands[0].Model).Vision {
		t.Fatalf("expected vision model first, got %+v", visionCands)
	}

	// Fast task should pick a fast provider/model (Groq).
	fastCands, _ := Route(chain, RouteRequest{
		Prompt:     "hello!",
		Preference: provider.Preference{Auto: true},
	})
	if len(fastCands) == 0 || fastCands[0].Provider != "groq" {
		t.Fatalf("expected groq to lead for fast prompt, got %+v", fastCands[0])
	}
}

func TestRaceReturnsFastestAndCancelsRest(t *testing.T) {
	var slowCanceled atomic.Bool

	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		time.Sleep(15 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "fast-1",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "fast winner"}},
			},
		})
	}))
	defer fastSrv.Close()

	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		select {
		case <-r.Context().Done():
			slowCanceled.Store(true)
			return
		case <-time.After(2 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "slow-1",
				"choices": []map[string]any{
					{"index": 0, "message": map[string]string{"role": "assistant", "content": "slow loser"}},
				},
			})
		}
	}))
	defer slowSrv.Close()

	chain, err := provider.NewChain([]provider.Config{
		{Name: "slow", BaseURL: slowSrv.URL, Models: []string{"slow-model"}},
		{Name: "fast", BaseURL: fastSrv.URL, Models: []string{"fast-model"}},
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	res, err := Race(context.Background(), chain, chain.Candidates(), openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: "ping"},
		},
	}, RaceOptions{MaxModels: 2, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Race failed: %v", err)
	}

	if res.Text != "fast winner" || res.Provider != "fast" {
		t.Errorf("unexpected race result: %+v", res)
	}

	// Give the cancelled request goroutine a moment to observe context cancellation.
	deadline := time.Now().Add(500 * time.Millisecond)
	for !slowCanceled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !slowCanceled.Load() {
		t.Error("expected slower racer context to be cancelled once winner completed")
	}
}
