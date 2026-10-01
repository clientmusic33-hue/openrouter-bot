// Package api talks to OpenAI-compatible providers and renders streamed
// completions into Telegram messages.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"openrouter-bot/config"
	"openrouter-bot/provider"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sashabaranov/go-openai"
)

// modelsTimeout bounds the /models lookup.
const modelsTimeout = 20 * time.Second

// modelsCacheTTL is how long the free models list is cached in memory.
const modelsCacheTTL = 5 * time.Minute

type cachedModelsEntry struct {
	result    string
	expiresAt time.Time
}

var (
	modelsCacheMu sync.RWMutex
	modelsCache   = make(map[string]cachedModelsEntry)
)

type Pricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

type Model struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	Pricing     Pricing `json:"pricing"`
}

type APIResponse struct {
	Data []Model `json:"data"`
}

// -----------------------------------------------------------------------------
// GET FREE MODELS
// -----------------------------------------------------------------------------

// GetFreeModels returns a Markdown list of models that are free for both
// prompt and completion tokens. Results are cached with a TTL to avoid
// redundant upstream API calls.
func GetFreeModels(baseURL string, apiKey string) (string, error) {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		return "", errors.New("no provider base url configured")
	}

	now := time.Now()
	modelsCacheMu.RLock()
	if entry, ok := modelsCache[base]; ok && now.Before(entry.expiresAt) {
		modelsCacheMu.RUnlock()
		return entry.result, nil
	}
	modelsCacheMu.RUnlock()

	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		return "", fmt.Errorf("error building models request: %w", err)
	}
	// The endpoint is public, but sending the key avoids stricter rate limits.
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := provider.SharedHTTPClient(modelsTimeout)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error get models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("models API returned status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("error read response: %w", err)
	}

	var apiResponse APIResponse
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return "", fmt.Errorf("error parse json: %w", err)
	}

	var result strings.Builder
	for _, model := range apiResponse.Data {
		if isFreeModel(model) {
			result.WriteString(fmt.Sprintf("➡ `%s`\n", model.ID))
		}
	}

	if result.Len() == 0 {
		return "", errors.New("no free models returned by the API")
	}

	rendered := result.String()
	modelsCacheMu.Lock()
	modelsCache[base] = cachedModelsEntry{
		result:    rendered,
		expiresAt: time.Now().Add(modelsCacheTTL),
	}
	modelsCacheMu.Unlock()

	return rendered, nil
}

// isFreeModel reports whether both prompt and completion are free. Checking
// only the prompt price admits models that charge for generated tokens.
func isFreeModel(model Model) bool {
	return isZeroPrice(model.Pricing.Prompt) && isZeroPrice(model.Pricing.Completion)
}

func isZeroPrice(price string) bool {
	price = strings.TrimSpace(price)
	if price == "" {
		return false
	}

	value, err := strconv.ParseFloat(price, 64)
	return err == nil && value == 0
}

// -----------------------------------------------------------------------------
// USER MESSAGE
// -----------------------------------------------------------------------------

// buildUserMessage returns the message to send to the model plus the plain
// text to store in the conversation history.
func buildUserMessage(
	bot *tgbotapi.BotAPI,
	prompt Prompt,
	cfg *config.Config,
) (openai.ChatCompletionMessage, string) {
	if cfg.Vision && prompt.Message != nil {
		return addVisionMessage(bot, prompt.Message, cfg)
	}

	text := strings.TrimSpace(prompt.Text)

	return openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: text,
	}, text
}

// messageText returns the text, or the caption for a media message. The
// Telegram helper deliberately leaves Caption empty for text messages, so a
// single accessor keeps the call sites honest.
func messageText(message *tgbotapi.Message) string {
	if message == nil {
		return ""
	}
	if message.Text != "" {
		return message.Text
	}

	return message.Caption
}

// -----------------------------------------------------------------------------
// VISION MESSAGE
// -----------------------------------------------------------------------------

// hasImageAttachment reports whether message carries a photo or an image document.
func hasImageAttachment(message *tgbotapi.Message) bool {
	if message == nil {
		return false
	}
	if len(message.Photo) > 0 {
		return true
	}
	if message.Document != nil && strings.HasPrefix(strings.ToLower(message.Document.MimeType), "image/") {
		return true
	}
	return false
}

func addVisionMessage(
	bot *tgbotapi.BotAPI,
	message *tgbotapi.Message,
	cfg *config.Config,
) (openai.ChatCompletionMessage, string) {
	plainText := strings.TrimSpace(messageText(message))

	fileID := ""
	if len(message.Photo) > 0 {
		// Use the largest available photo size.
		photoSize := message.Photo[len(message.Photo)-1]
		fileID = photoSize.FileID
	} else if message.Document != nil && strings.HasPrefix(strings.ToLower(message.Document.MimeType), "image/") {
		fileID = message.Document.FileID
	}

	if fileID == "" || bot == nil {
		return openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: plainText,
		}, plainText
	}

	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		log.Printf("Error getting file: %v", err)

		return openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: plainText,
		}, plainText
	}

	prompt := plainText
	if prompt == "" {
		prompt = cfg.VisionPrompt
	}
	if prompt == "" {
		prompt = "Describe this image, extract any visible text (OCR), and summarize key details."
	}

	visionMessage := openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser,
		MultiContent: []openai.ChatMessagePart{
			{
				Type: openai.ChatMessagePartTypeText,
				Text: prompt,
			},
			{
				Type: openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{
					URL:    file.Link(bot.Token),
					Detail: openai.ImageURLDetail(cfg.VisionDetails),
				},
			},
		},
	}

	return visionMessage, prompt
}
