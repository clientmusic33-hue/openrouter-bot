// Package translator turns free-form text into another language using the
// configured chat model.
package translator

import (
	"context"
	"fmt"
	"strings"

	"github.com/sashabaranov/go-openai"
)

// maxOutputTokens bounds the translation so that a pathological input cannot
// produce an enormous (and expensive) response.
const maxOutputTokens = 2000

// Translate translates text into the requested target language.
//
// The caller owns ctx and should bound it with a timeout - a translation is a
// normal network request and can otherwise hang forever.
func Translate(
	ctx context.Context,
	client *openai.Client,
	text string,
	targetLanguage string,
	model string,
) (string, error) {
	if client == nil {
		return "", fmt.Errorf("no AI client configured")
	}

	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("empty text")
	}

	targetLanguage = strings.TrimSpace(targetLanguage)
	if targetLanguage == "" {
		return "", fmt.Errorf("target language is empty")
	}

	prompt := fmt.Sprintf(
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

	resp, err := client.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: model,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: "You are a translation-only assistant.",
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: prompt,
				},
			},
			Temperature: 0,
			MaxTokens:   maxOutputTokens,
		},
	)
	if err != nil {
		return "", fmt.Errorf("translation request failed: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("translation returned no response")
	}

	result := strings.TrimSpace(resp.Choices[0].Message.Content)
	if result == "" {
		return "", fmt.Errorf("translation returned empty response")
	}

	return result, nil
}
