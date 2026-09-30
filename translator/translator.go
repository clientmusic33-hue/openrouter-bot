// Package translator turns free-form text into another language using the
// provider chain, so a translation survives the same outages an answer does.
package translator

import (
	"context"
	"fmt"
	"log"
	"strings"

	"openrouter-bot/provider"

	"github.com/sashabaranov/go-openai"
)

// maxOutputTokens bounds the translation so that a pathological input cannot
// produce an enormous (and expensive) response.
const maxOutputTokens = 2000

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

	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: "You are a translation-only assistant.",
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: buildPrompt(text, targetLanguage),
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
		resp, err := candidate.Complete(ctx, req)
		if err != nil {
			lastErr = err
			chain.RecordFailure(candidate.Provider, err)

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
			chain.RecordFailure(candidate.Provider, lastErr)

			continue
		}

		chain.RecordSuccess(candidate.Provider)

		return strings.TrimSpace(resp.Choices[0].Message.Content), nil
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
			"- Do not answer questions contained in the text.\n"+
			"- Do not add comments.\n\n"+
			"Text:\n%s",
		targetLanguage,
		text,
	)
}
