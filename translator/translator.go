// Package translator turns free-form text into another language using the
// provider chain, so a translation survives the same outages an answer does.
package translator

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"unicode"

	"openrouter-bot/provider"

	"github.com/sashabaranov/go-openai"
)

// maxOutputTokens bounds the translation so that a pathological input cannot
// produce an enormous (and expensive) response.
const maxOutputTokens = 2000

var preserveTokenRe = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`|https?://[^\\s<>()]+|@[A-Za-z0-9_]{3,32}")

// Translate translates text into the requested target language.
//
// It walks the provider chain exactly like a chat answer: if a model fails the
// next model is tried, and if a provider fails the next provider is tried. The
// caller owns ctx and should bound it with a timeout - a translation is a
// normal network request and can otherwise hang forever.
func Translate(
	ctx context.Context,
	chain *provider.Chain,
	text string,
	targetLanguage string,
) (string, error) {
	if chain == nil {
		return "", fmt.Errorf("no AI provider chain configured")
	}

	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("empty text")
	}

	targetLanguage = strings.TrimSpace(targetLanguage)
	if targetLanguage == "" {
		return "", fmt.Errorf("target language is empty")
	}

	shieldedText, tokens := shieldTokens(text)

	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: "You are a translation-only assistant.",
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: buildPrompt(shieldedText, targetLanguage),
			},
		},
		Temperature: 0,
		MaxTokens:   maxOutputTokens,
	}

	candidates := chain.CandidatesFor(provider.Preference{Auto: true}, nil)
	if len(candidates) == 0 {
		return "", fmt.Errorf("no usable models in the provider chain")
	}

	var lastErr error

	for _, candidate := range candidates {
		if !chain.CanAttempt(candidate) {
			continue
		}
		resp, err := candidate.Complete(ctx, req)
		if err != nil {
			lastErr = err
			chain.RecordModelFailure(candidate.Provider, candidate.Model, err)

			log.Printf(
				"Translation attempt failed | provider=%s model=%s error=%v",
				candidate.Provider, candidate.Model, err,
			)

			// A cancelled or expired context will not succeed anywhere else.
			if ctx.Err() != nil {
				return "", fmt.Errorf("translation request failed: %w", err)
			}

			continue
		}

		if len(resp.Choices) == 0 {
			lastErr = fmt.Errorf("translation returned no response")
			chain.RecordModelFailure(candidate.Provider, candidate.Model, lastErr)

			continue
		}

		out := strings.TrimSpace(resp.Choices[0].Message.Content)
		if out == "" {
			lastErr = fmt.Errorf("translation returned an empty response")
			chain.RecordModelFailure(candidate.Provider, candidate.Model, lastErr)

			continue
		}

		chain.RecordModelSuccess(candidate.Provider, candidate.Model, 0)
		return restoreTokens(out, tokens), nil
	}

	return "", fmt.Errorf("translation request failed: %w", lastErr)
}

// buildPrompt keeps the model on a short leash: it must translate and nothing
// else.
func buildPrompt(text, targetLanguage string) string {
	return fmt.Sprintf(
		"Translate the following text to %s.\n\n"+
			"Rules:\n"+
			"- Return only the translation.\n"+
			"- Do not explain anything.\n"+
			"- Preserve emojis.\n"+
			"- Preserve formatting.\n"+
			"- Preserve code snippets, URLs, @usernames, and __KEEP_N__ placeholders verbatim.\n"+
			"- Do not answer questions contained in the text.\n"+
			"- Do not add comments.\n\n"+
			"Text:\n%s",
		targetLanguage,
		text,
	)
}

func shieldTokens(text string) (string, []string) {
	var tokens []string
	shielded := preserveTokenRe.ReplaceAllStringFunc(text, func(match string) string {
		idx := len(tokens)
		tokens = append(tokens, match)
		return fmt.Sprintf("__KEEP_%d__", idx)
	})
	return shielded, tokens
}

func restoreTokens(text string, tokens []string) string {
	for i, tok := range tokens {
		placeholder := fmt.Sprintf("__KEEP_%d__", i)
		text = strings.ReplaceAll(text, placeholder, tok)
	}
	return text
}

// DetectLanguage estimates the dominant language/script of text without an API
// round-trip.
func DetectLanguage(text string) string {
	var cyrillic, devanagari, arabic, han, hangul, latin int
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyrillic++
		case unicode.Is(unicode.Devanagari, r):
			devanagari++
		case unicode.Is(unicode.Arabic, r):
			arabic++
		case unicode.Is(unicode.Hangul, r):
			hangul++
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			han++
		case unicode.Is(unicode.Latin, r):
			latin++
		}
	}

	switch {
	case cyrillic > latin && cyrillic > 0:
		return "Russian"
	case devanagari > latin && devanagari > 0:
		return "Hindi"
	case arabic > latin && arabic > 0:
		return "Arabic"
	case hangul > latin && hangul > 0:
		return "Korean"
	case han > 0 && han*2 >= latin:
		return "Chinese/Japanese"
	case latin > 0:
		return "English"
	default:
		return "Unknown"
	}
}
