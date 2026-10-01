package translator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openrouter-bot/provider"
)

// chatServer answers /chat/completions with the supplied content, or fails
// every request when status is set.
func chatServer(t *testing.T, content string, status int) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			http.Error(w, "boom", status)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gen-1",
			"choices": []map[string]any{
				{
					"index":         0,
					"message":       map[string]string{"role": "assistant", "content": content},
					"finish_reason": "stop",
				},
			},
		})
	}))
}

func newChain(t *testing.T, cfgs ...provider.Config) *provider.Chain {
	t.Helper()

	chain, err := provider.NewChain(cfgs)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	return chain
}

func TestTranslateUsesTheFirstHealthyProvider(t *testing.T) {
	good := chatServer(t, "  Hello  ", 0)
	defer good.Close()

	chain := newChain(t, provider.Config{
		Name:    "good",
		BaseURL: good.URL,
		Models:  []string{"good-model"},
	})

	got, err := Translate(context.Background(), chain, "Hallo", "English")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	if got != "Hello" {
		t.Errorf("translation = %q, want %q", got, "Hello")
	}
}

// TestTranslateFailsOver is the contract that a translation survives a broken
// provider: the answer comes from the next one instead of an error message.
func TestTranslateFailsOver(t *testing.T) {
	broken := chatServer(t, "", http.StatusInternalServerError)
	defer broken.Close()

	good := chatServer(t, "Hallo", 0)
	defer good.Close()

	chain := newChain(t,
		provider.Config{Name: "broken", BaseURL: broken.URL, Models: []string{"a"}},
		provider.Config{Name: "good", BaseURL: good.URL, Models: []string{"b"}},
	)

	got, err := Translate(context.Background(), chain, "Hello", "German")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	if got != "Hallo" {
		t.Errorf("translation = %q, want the next provider's answer", got)
	}

	// The failing provider must be blamed, so the next request skips it.
	for _, info := range chain.Status() {
		if info.Name == "broken" && info.Failures == 0 {
			t.Error("the failing provider was not recorded")
		}
	}
}

func TestTranslateRejectsBadInput(t *testing.T) {
	server := chatServer(t, "x", 0)
	defer server.Close()

	chain := newChain(t, provider.Config{Name: "p", BaseURL: server.URL, Models: []string{"m"}})

	cases := map[string]struct {
		chain *provider.Chain
		text  string
		lang  string
	}{
		"no chain":     {nil, "Hello", "English"},
		"empty text":   {chain, "   ", "English"},
		"empty target": {chain, "Hello", "  "},
	}

	for name, tc := range cases {
		if _, err := Translate(context.Background(), tc.chain, tc.text, tc.lang); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestTranslatePromptIsStrict keeps the instruction set that stops a model
// from answering the message instead of translating it.
func TestTranslatePromptIsStrict(t *testing.T) {
	prompt := buildPrompt("Hello", "German")

	for _, want := range []string{"Translate the following text to German", "Return only the translation", "Hello"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

func TestShieldAndRestoreTokensAndDetectLanguage(t *testing.T) {
	orig := "Hello @alice_dev check `go test ./...` at https://example.com/docs 🚀"
	shielded, tokens := shieldTokens(orig)
	if len(tokens) != 3 {
		t.Fatalf("expected 3 shielded tokens, got %d (%v)", len(tokens), tokens)
	}
	if strings.Contains(shielded, "@alice_dev") || strings.Contains(shielded, "https://example.com/docs") {
		t.Errorf("tokens were not shielded: %q", shielded)
	}
	restored := restoreTokens(shielded, tokens)
	if restored != orig {
		t.Errorf("restoreTokens() = %q, want %q", restored, orig)
	}

	if got := DetectLanguage("Привет мир"); got != "Russian" {
		t.Errorf("DetectLanguage(Russian) = %q", got)
	}
	if got := DetectLanguage("नमस्ते दुनिया"); got != "Hindi" {
		t.Errorf("DetectLanguage(Hindi) = %q", got)
	}
	if got := DetectLanguage("Hello world"); got != "English" {
		t.Errorf("DetectLanguage(English) = %q", got)
	}
}
