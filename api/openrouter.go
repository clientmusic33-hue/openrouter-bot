// Package api talks to OpenAI-compatible providers and renders streamed
// completions into Telegram messages.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"openrouter-bot/config"
	"openrouter-bot/lang"
	"openrouter-bot/provider"
	"openrouter-bot/user"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sashabaranov/go-openai"
)

// editInterval throttles Telegram edits while streaming. Telegram API calls
// are relatively expensive; 350ms feels responsive without hammering it.
const editInterval = 350 * time.Millisecond

// maxChunks caps how many Telegram messages a single answer may occupy.
const maxChunks = 20

// maxChunksDefault is used when SplitMessage is called without a cap.
const maxChunksDefault = 20

// modelsTimeout bounds the /models lookup.
const modelsTimeout = 20 * time.Second

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
// prompt and completion tokens.
func GetFreeModels(baseURL string, apiKey string) (string, error) {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		return "", errors.New("no provider base url configured")
	}

	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		return "", fmt.Errorf("error building models request: %w", err)
	}
	// The endpoint is public, but sending the key avoids stricter rate limits.
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: modelsTimeout}

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

	return result.String(), nil
}

// isFreeModel reports whether both prompt and completion are free. Checking
// only the prompt price admits models that charge for generated tokens.
func isFreeModel(model Model) bool {
	return isZeroPrice(model.Pricing.Prompt) && isZeroPrice(model.Pricing.Completion)
}

func isZeroPrice(price string) bool {
	price = strings.TrimSpace(price)

	return price == "" || price == "0" || strings.Trim(price, "0.") == ""
}

// -----------------------------------------------------------------------------
// MAIN CHAT HANDLER
// -----------------------------------------------------------------------------

// HandleChatGPTStreamResponse streams a completion into Telegram, editing the
// placeholder message as tokens arrive.
//
// It walks the provider chain: if a model fails the next model is tried, and
// if a provider fails the next provider is tried - both at stream creation
// and mid-stream.
//
// It returns the OpenRouter generation id (empty for other providers), the
// name of the provider that served the answer, and an error.
func HandleChatGPTStreamResponse(
	parentCtx context.Context,
	bot *tgbotapi.BotAPI,
	chain *provider.Chain,
	message *tgbotapi.Message,
	cfg *config.Config,
	tracker *user.UsageTracker,
) (string, string, error) {
	if cfg == nil {
		return "", "", errors.New("no configuration provided")
	}
	if chain == nil {
		return "", "", errors.New("no provider chain configured")
	}

	// One active generation per user. The lock is held for the whole request.
	tracker.ChatMu.Lock()
	defer tracker.ChatMu.Unlock()

	candidates := chain.Candidates()
	if len(candidates) == 0 {
		return "", "", errors.New("no models available in the provider chain")
	}

	loadMessage := lang.Translate("loadText", cfg.Lang)
	errorMessage := lang.Translate("errorText", cfg.Lang)

	// ---------------------------------------------------------------------
	// SEND INITIAL MESSAGE
	// ---------------------------------------------------------------------

	processingMsg := tgbotapi.NewMessage(message.Chat.ID, loadMessage)
	sentMsg, err := bot.Send(processingMsg)
	if err != nil {
		return "", "", fmt.Errorf("failed to send processing message: %w", err)
	}
	lastMessageID := sentMsg.MessageID

	// ---------------------------------------------------------------------
	// LOADING ANIMATION
	// ---------------------------------------------------------------------

	stopAnimation := make(chan bool, 1)
	var animationDone sync.WaitGroup
	animationDone.Add(1)

	go func() {
		defer animationDone.Done()
		runLoadingAnimation(bot, message.Chat.ID, lastMessageID, loadMessage, stopAnimation)
	}()

	// ---------------------------------------------------------------------
	// BUILD CONVERSATION
	// ---------------------------------------------------------------------

	tracker.CheckHistory(cfg.MaxHistorySize, cfg.MaxHistoryTime)
	tracker.Touch()

	currentMessage, userText := buildUserMessage(bot, message, cfg)

	messages := []openai.ChatCompletionMessage{
		{
			Role:    openai.ChatMessageRoleSystem,
			Content: tracker.GetSystemPrompt(),
		},
	}
	for _, historyMessage := range tracker.GetMessages() {
		if strings.TrimSpace(historyMessage.Content) == "" {
			continue
		}
		messages = append(messages, openai.ChatCompletionMessage{
			Role:    historyMessage.Role,
			Content: historyMessage.Content,
		})
	}
	messages = append(messages, currentMessage)

	req := openai.ChatCompletionRequest{
		FrequencyPenalty: float32(cfg.Model.FrequencyPenalty),
		PresencePenalty:  float32(cfg.Model.PresencePenalty),
		Temperature:      float32(cfg.Model.Temperature),
		TopP:             float32(cfg.Model.TopP),
		MaxTokens:        cfg.MaxTokens,
		Messages:         messages,
		Stream:           true,
	}

	// ---------------------------------------------------------------------
	// REQUEST CONTEXT
	// ---------------------------------------------------------------------
	//
	// Two independent deadlines:
	//
	//   * idle  - aborts a stream that stops delivering chunks
	//   * total - bounds the whole request, including a slow first token
	//
	// Deriving from parentCtx means a shutdown cancels in-flight requests.

	ctx, cancel := context.WithTimeout(parentCtx, cfg.RequestTimeout)
	defer cancel()

	idleTimer := time.AfterFunc(cfg.StreamIdleTimeout, cancel)
	defer idleTimer.Stop()

	// ---------------------------------------------------------------------
	// OPEN THE STREAM, WALKING THE CHAIN
	// ---------------------------------------------------------------------

	idx := 0

	var stream *openai.ChatCompletionStream
	var current provider.Candidate
	var streamErr error

	for ; idx < len(candidates); idx++ {
		candidate := candidates[idx]

		stream, streamErr = candidate.Stream(ctx, req)
		if streamErr == nil {
			current = candidate
			chain.RecordSuccess(candidate.Provider)
			break
		}

		chain.RecordFailure(candidate.Provider, streamErr)
		log.Printf(
			"Stream creation failed | provider=%s model=%s error=%v",
			candidate.Provider, candidate.Model, streamErr,
		)
	}

	if stream == nil {
		stopLoading(stopAnimation)
		animationDone.Wait()

		log.Printf("Every provider in the chain failed: %v", streamErr)

		editMsg := tgbotapi.NewEditMessageText(message.Chat.ID, lastMessageID, errorMessage)
		if _, editErr := bot.Send(editMsg); editErr != nil {
			log.Printf("Failed to send error message: %v", editErr)
		}

		return "", "", streamErr
	}

	// Close whichever stream is current when we return: a mid-stream failover
	// replaces it, so a plain `defer stream.Close()` would close the wrong one
	// and leak the replacement.
	currentStream := stream
	defer func() {
		if currentStream != nil {
			_ = currentStream.Close()
		}
	}()

	tracker.SetStream(currentStream, cancel)
	defer tracker.ClearStream()

	stopLoading(stopAnimation)
	// Wait for the animation goroutine to finish so that a late
	// "Processing request..." edit cannot overwrite the first tokens.
	animationDone.Wait()

	log.Printf(
		"⚡ Stream started | provider=%s | model=%s | user=%s",
		current.Provider, current.Model, tracker.UserName,
	)

	sink := newMessageSink(bot, message.Chat.ID, lastMessageID)

	// ---------------------------------------------------------------------
	// STREAM LOOP
	// ---------------------------------------------------------------------

	var messageText string
	responseID := ""
	lastEdit := time.Now()
	firstEdit := true

	for {
		response, recvErr := currentStream.Recv()

		// Rearm the idle watchdog: any activity proves the stream is alive.
		idleTimer.Reset(cfg.StreamIdleTimeout)

		if responseID == "" && response.ID != "" {
			responseID = response.ID
		}

		// ---------------------------------------------------------------
		// NORMAL STREAM END
		// ---------------------------------------------------------------

		if errors.Is(recvErr, io.EOF) {
			log.Printf(
				"Stream finished | provider=%s | model=%s | response=%s",
				current.Provider, current.Model, responseID,
			)

			chain.RecordSuccess(current.Provider)

			tracker.AddMessage(openai.ChatMessageRoleUser, userText)
			tracker.AddMessage(openai.ChatMessageRoleAssistant, messageText)
			sink.Finish(messageText)

			return responseID, current.Provider, nil
		}

		// ---------------------------------------------------------------
		// STREAM ERROR
		// ---------------------------------------------------------------

		if recvErr != nil {
			// Stopped with /stop: keep what was generated so far.
			if errors.Is(recvErr, context.Canceled) {
				log.Printf("Stream cancelled by user | user=%s", tracker.UserName)

				tracker.AddMessage(openai.ChatMessageRoleUser, userText)
				if strings.TrimSpace(messageText) != "" {
					tracker.AddMessage(openai.ChatMessageRoleAssistant, messageText)
					sink.Finish(messageText)
				}

				return responseID, current.Provider, nil
			}

			log.Printf(
				"Stream error | provider=%s | model=%s | error=%v",
				current.Provider, current.Model, recvErr,
			)

			chain.RecordFailure(current.Provider, recvErr)

			// ---------------------------------------------------------
			// FAILOVER TO THE NEXT MODEL / PROVIDER
			// ---------------------------------------------------------
			//
			// Any partial text is kept, so the user sees a continuous
			// answer rather than losing what already arrived.

			if next, nextCandidate, nextErr := openNext(
				ctx, chain, candidates, idx+1, req,
			); nextErr == nil {
				_ = currentStream.Close()

				currentStream = next
				current = nextCandidate
				idx = indexOf(candidates, nextCandidate)

				tracker.SetStream(currentStream, cancel)

				log.Printf(
					"⚡ Failover to provider=%s model=%s",
					current.Provider, current.Model,
				)

				continue
			}

			// ---------------------------------------------------------
			// KEEP PARTIAL OUTPUT
			// ---------------------------------------------------------

			if strings.TrimSpace(messageText) != "" {
				tracker.AddMessage(openai.ChatMessageRoleUser, userText)
				tracker.AddMessage(openai.ChatMessageRoleAssistant, messageText)
				sink.Finish(messageText)

				return responseID, current.Provider, nil
			}

			editMsg := tgbotapi.NewEditMessageText(message.Chat.ID, lastMessageID, errorMessage)
			if _, editErr := bot.Send(editMsg); editErr != nil {
				log.Printf("Failed to send stream error: %v", editErr)
			}

			return responseID, current.Provider, recvErr
		}

		// ---------------------------------------------------------------
		// COLLECT TOKEN
		// ---------------------------------------------------------------

		if len(response.Choices) == 0 {
			continue
		}

		content := response.Choices[0].Delta.Content
		if content == "" {
			continue
		}

		messageText += content

		if strings.TrimSpace(messageText) == "" {
			continue
		}

		// Send the first visible token immediately for a responsive feel.
		if firstEdit || time.Since(lastEdit) >= editInterval {
			if sink.Update(messageText) {
				lastEdit = time.Now()
				firstEdit = false
			}
		}
	}
}

// openNext tries every candidate from start onwards and returns the first
// stream that opens.
func openNext(
	ctx context.Context,
	chain *provider.Chain,
	candidates []provider.Candidate,
	start int,
	req openai.ChatCompletionRequest,
) (*openai.ChatCompletionStream, provider.Candidate, error) {
	for i := start; i < len(candidates); i++ {
		candidate := candidates[i]

		stream, err := candidate.Stream(ctx, req)
		if err == nil {
			chain.RecordSuccess(candidate.Provider)

			return stream, candidate, nil
		}

		chain.RecordFailure(candidate.Provider, err)
		log.Printf(
			"Failover attempt failed | provider=%s model=%s error=%v",
			candidate.Provider, candidate.Model, err,
		)
	}

	return nil, provider.Candidate{}, errors.New("no further candidates")
}

// indexOf returns the position of a candidate in the list.
func indexOf(candidates []provider.Candidate, target provider.Candidate) int {
	for i, candidate := range candidates {
		if candidate.Provider == target.Provider && candidate.Model == target.Model {
			return i
		}
	}

	return 0
}

// -----------------------------------------------------------------------------
// USER MESSAGE
// -----------------------------------------------------------------------------

// buildUserMessage returns the message to send to the model plus the plain
// text to store in the conversation history.
func buildUserMessage(
	bot *tgbotapi.BotAPI,
	message *tgbotapi.Message,
	cfg *config.Config,
) (openai.ChatCompletionMessage, string) {
	if cfg.Vision {
		return addVisionMessage(bot, message, cfg)
	}

	text := strings.TrimSpace(message.Text)

	return openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: text,
	}, text
}

// -----------------------------------------------------------------------------
// LOADING ANIMATION
// -----------------------------------------------------------------------------

func runLoadingAnimation(
	bot *tgbotapi.BotAPI,
	chatID int64,
	messageID int,
	loadMessage string,
	stop chan bool,
) {
	dots := []string{"", ".", "..", "..."}

	ticker := time.NewTicker(1200 * time.Millisecond)
	defer ticker.Stop()

	for i := 0; ; i = (i + 1) % len(dots) {
		select {
		case <-stop:
			return
		case <-ticker.C:
			editMsg := tgbotapi.NewEditMessageText(chatID, messageID, loadMessage+dots[i])
			if _, err := bot.Send(editMsg); err != nil {
				if !strings.Contains(err.Error(), "message is not modified") {
					log.Printf("Loading edit error: %v", err)
				}
			}
		}
	}
}

func stopLoading(stopAnimation chan bool) {
	select {
	case stopAnimation <- true:
	default:
	}
}

// -----------------------------------------------------------------------------
// CHUNKED MESSAGE RENDERING
// -----------------------------------------------------------------------------

// messageSink renders a growing answer across as many Telegram messages as it
// needs. Telegram caps a single message at 4096 characters, and that limit is
// enforced on edits too, so a long answer has to spill into new messages.
type messageSink struct {
	bot        *tgbotapi.BotAPI
	chatID     int64
	messageIDs []int
	rendered   []string
	limit      int
	maxChunks  int
}

func newMessageSink(bot *tgbotapi.BotAPI, chatID int64, firstMessageID int) *messageSink {
	return &messageSink{
		bot:        bot,
		chatID:     chatID,
		messageIDs: []int{firstMessageID},
		rendered:   []string{""},
		limit:      config.ChunkLimit,
		maxChunks:  maxChunks,
	}
}

// Update renders text, sending extra messages when it outgrows the current
// ones. It reports whether anything changed.
func (s *messageSink) Update(text string) bool {
	chunks := SplitMessage(text, s.limit, s.maxChunks)
	if len(chunks) == 0 {
		return false
	}

	changed := false

	for i, chunk := range chunks {
		if i >= len(s.messageIDs) {
			msg := tgbotapi.NewMessage(s.chatID, chunk)
			sent, err := s.bot.Send(msg)
			if err != nil {
				log.Printf("Failed to send continuation message: %v", err)
				return changed
			}
			s.messageIDs = append(s.messageIDs, sent.MessageID)
			s.rendered = append(s.rendered, chunk)
			changed = true

			continue
		}

		if chunk == s.rendered[i] {
			continue
		}

		if editMessage(s.bot, s.chatID, s.messageIDs[i], chunk, "") {
			s.rendered[i] = chunk
			changed = true
		}
	}

	return changed
}

// Finish writes the final text and upgrades formatting, trying MarkdownV2 and
// then legacy Markdown before settling for plain text.
func (s *messageSink) Finish(text string) {
	s.Update(text)

	chunks := SplitMessage(text, s.limit, s.maxChunks)

	for i, chunk := range chunks {
		if i >= len(s.messageIDs) {
			return
		}

		// Markdown is only applied at the end: a half-written "**bold" would
		// be rejected by Telegram while streaming.
		if editMessage(s.bot, s.chatID, s.messageIDs[i], chunk, tgbotapi.ModeMarkdownV2) {
			continue
		}
		if editMessage(s.bot, s.chatID, s.messageIDs[i], chunk, tgbotapi.ModeMarkdown) {
			continue
		}

		// Leave the plain text version that Update already wrote.
		log.Printf("Markdown rendering failed for chunk %d, keeping plain text", i)
	}
}

func editMessage(
	bot *tgbotapi.BotAPI,
	chatID int64,
	messageID int,
	text string,
	parseMode string,
) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	if parseMode != "" {
		editMsg.ParseMode = parseMode
	}

	if _, err := bot.Send(editMsg); err != nil {
		if !strings.Contains(err.Error(), "message is not modified") {
			log.Printf("Telegram edit error (parse_mode=%q): %v", parseMode, err)
		}
		return false
	}

	return true
}

// SplitMessage breaks text into chunks of at most limit runes, preferring
// newline boundaries so that paragraphs and code blocks stay intact.
func SplitMessage(text string, limit int, maxChunks int) []string {
	if limit <= 0 {
		limit = config.ChunkLimit
	}
	if maxChunks <= 0 {
		maxChunks = maxChunksDefault
	}

	text = strings.TrimRight(text, " \t\n")
	if text == "" {
		return nil
	}

	runes := []rune(text)
	if len(runes) <= limit {
		return []string{text}
	}

	var chunks []string

	for start := 0; start < len(runes); {
		if len(chunks) == maxChunks-1 {
			// Last allowed chunk: keep the tail and mark the truncation.
			tail := strings.TrimRight(string(runes[start:]), " \t\n")
			chunks = append(chunks, tail+"\n\n… (truncated)")
			break
		}

		end := start + limit
		if end >= len(runes) {
			chunks = append(chunks, strings.TrimRight(string(runes[start:]), " \t\n"))
			break
		}

		cut := -1
		for i := end; i > start+limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i
				break
			}
		}
		if cut <= 0 {
			cut = end
		}

		chunk := strings.TrimRight(string(runes[start:cut]), " \t\n")
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		start = cut
	}

	if len(chunks) == 0 {
		return nil
	}

	return chunks
}

// -----------------------------------------------------------------------------
// VISION MESSAGE
// -----------------------------------------------------------------------------

func addVisionMessage(
	bot *tgbotapi.BotAPI,
	message *tgbotapi.Message,
	cfg *config.Config,
) (openai.ChatCompletionMessage, string) {
	plainText := strings.TrimSpace(message.Text)

	if len(message.Photo) == 0 {
		return openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: plainText,
		}, plainText
	}

	// Use the largest available photo size.
	photoSize := message.Photo[len(message.Photo)-1]

	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: photoSize.FileID})
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
		prompt = "Describe this image."
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
