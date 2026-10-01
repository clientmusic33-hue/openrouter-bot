package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"openrouter-bot/config"
	"openrouter-bot/lang"
	"openrouter-bot/provider"
	"openrouter-bot/router"
	"openrouter-bot/ui"
	"openrouter-bot/user"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sashabaranov/go-openai"
)

// Options describe how one generation should behave for one user.
type Options struct {
	// Preference is the user's routing choice: a pinned model, a pinned
	// provider, or automatic selection.
	Preference provider.Preference
	// Profile is the usage profile used to score models in auto mode. A nil
	// profile keeps the configured chain order.
	Profile *provider.UsageProfile
	// ShowFooter appends "which model answered" to the message.
	ShowFooter bool
	// Streaming edits the message while tokens arrive. When false the answer
	// appears in one piece, which is easier on very slow connections.
	Streaming bool
	// Markdown makes the final message try MarkdownV2. Off by default: the
	// bot is asked to answer in plain text.
	Markdown bool
	// MaxChars is the outgoing chunk size. Defaults to config.ChunkLimit.
	MaxChars int
	// Buttons attaches the stop / regenerate / feedback keyboard.
	Buttons bool
	// TaskOverride forces a specific router task category (e.g. FAST, CODING).
	TaskOverride router.TaskType
	// SystemSupplement appends extra context (persona, memory, summary) to the
	// system prompt for this request without mutating the stored prompt.
	SystemSupplement string
}

// Prompt is one generation request. It is either derived from an incoming
// message or replayed later, which is what "regenerate" does.
type Prompt struct {
	// ChatID is where the answer is rendered.
	ChatID int64
	// Text is the user's question. For a message with a caption or photo the
	// message itself supplies the full content.
	Text string
	// Message is the original update, when there was one. It carries photos
	// for vision and the reply context.
	Message *tgbotapi.Message
}

// Result describes what actually happened, so the caller can record state
// (for regenerate and feedback) without repeating the request.
type Result struct {
	Provider   string
	Model      string
	ResponseID string
	Text       string
	// Failovers counts how many other models were tried before this one.
	Failovers int
	// Stopped is true when the user pressed stop.
	Stopped bool
	// Latency is the total generation duration.
	Latency time.Duration
	// Messages are the Telegram message ids that carry the answer. The first
	// one holds the action keyboard.
	Messages []int
}

// HandleChatGPTStreamResponse streams a completion into Telegram, editing the
// placeholder message as tokens arrive.
//
// It walks the candidate list built from the user's preference: if a model
// fails the next model is tried, and if a provider fails the next provider is
// tried - both at stream creation and mid-stream.
func HandleChatGPTStreamResponse(
	parentCtx context.Context,
	bot *tgbotapi.BotAPI,
	chain *provider.Chain,
	message *tgbotapi.Message,
	cfg *config.Config,
	tracker *user.UsageTracker,
	opts Options,
) (Result, error) {
	if message == nil {
		return Result{}, errors.New("no message provided")
	}

	return Generate(parentCtx, bot, chain, Prompt{
		ChatID:  message.Chat.ID,
		Text:    messageText(message),
		Message: message,
	}, cfg, tracker, opts)
}

// Generate renders one answer into ChatID, walking the provider chain. It is
// used both for incoming messages and for regenerated answers.
func Generate(
	parentCtx context.Context,
	bot *tgbotapi.BotAPI,
	chain *provider.Chain,
	prompt Prompt,
	cfg *config.Config,
	tracker *user.UsageTracker,
	opts Options,
) (Result, error) {
	if cfg == nil {
		return Result{}, errors.New("no configuration provided")
	}
	if chain == nil {
		return Result{}, errors.New("no provider chain configured")
	}

	// One active generation per user. The lock is held for the whole request.
	tracker.ChatMu.Lock()
	defer tracker.ChatMu.Unlock()

	hasImages := cfg.Vision && hasImageAttachment(prompt.Message)
	var profileVal provider.UsageProfile
	if opts.Profile != nil {
		profileVal = *opts.Profile
	}

	var candidates []provider.Candidate
	if opts.Preference.Auto || opts.TaskOverride != "" || hasImages {
		candidates, _ = router.Route(chain, router.RouteRequest{
			Prompt:       prompt.Text,
			HasImages:    hasImages,
			TaskOverride: opts.TaskOverride,
			Preference:   opts.Preference,
			Profile:      profileVal,
		})
	} else {
		candidates = chain.CandidatesFor(opts.Preference, opts.Profile)
	}
	if len(candidates) == 0 {
		return Result{}, errors.New("no usable models in the provider chain")
	}

	loadMessage := lang.Translate("loadText", cfg.Lang)
	errorMessage := lang.Translate("errorText", cfg.Lang)

	if loadMessage == "" {
		loadMessage = "⏳ Thinking"
	}
	if errorMessage == "" {
		errorMessage = "❌ Could not answer right now, please try again."
	}

	// ---------------------------------------------------------------------
	// SEND INITIAL MESSAGE
	// ---------------------------------------------------------------------

	placeholder := tgbotapi.NewMessage(prompt.ChatID, loadMessage)
	if opts.Buttons {
		placeholder.ReplyMarkup = ui.StopButton()
	}

	sentMsg, err := bot.Send(placeholder)
	if err != nil {
		return Result{}, fmt.Errorf("failed to send processing message: %w", err)
	}
	lastMessageID := sentMsg.MessageID

	stopTyping := typingLoop(bot, prompt.ChatID)
	defer stopTyping()

	stopAnimation := make(chan bool, 1)
	animationDone := make(chan struct{})

	go func() {
		defer close(animationDone)
		if opts.Streaming {
			runLoadingAnimation(bot, prompt.ChatID, lastMessageID, loadMessage, stopAnimation)
		}
	}()

	// ---------------------------------------------------------------------
	// BUILD CONVERSATION
	// ---------------------------------------------------------------------

	tracker.CheckHistory(cfg.MaxHistorySize, cfg.MaxHistoryTime)
	tracker.Touch()

	currentMessage, userText := buildUserMessage(bot, prompt, cfg)

	sysContent := tracker.GetSystemPrompt()
	if extra := strings.TrimSpace(opts.SystemSupplement); extra != "" {
		sysContent = strings.TrimSpace(sysContent + "\n\n" + extra)
	}

	messages := []openai.ChatCompletionMessage{
		{
			Role:    openai.ChatMessageRoleSystem,
			Content: sysContent,
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
	// OPEN THE STREAM, WALKING THE CANDIDATES
	// ---------------------------------------------------------------------

	idx := 0
	failovers := 0
	requestStart := time.Now()

	var stream *openai.ChatCompletionStream
	var current provider.Candidate
	var streamErr error

	for ; idx < len(candidates); idx++ {
		candidate := candidates[idx]
		attemptStart := time.Now()

		stream, streamErr = candidate.Stream(ctx, req)
		if streamErr == nil {
			if idx > 0 {
				failovers = idx
			}
			current = candidate
			chain.RecordSuccessLatency(candidate.Provider, candidate.Model, time.Since(attemptStart))
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
		<-animationDone

		log.Printf("Every provider in the chain failed: %v", streamErr)

		editMsg := tgbotapi.NewEditMessageText(prompt.ChatID, lastMessageID, errorMessage)
		if _, editErr := bot.Send(editMsg); editErr != nil {
			log.Printf("Failed to send error message: %v", editErr)
		}

		return Result{}, streamErr
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
	// "Thinking..." edit cannot overwrite the first tokens.
	<-animationDone

	log.Printf(
		"⚡ Stream started | provider=%s | model=%s | user=%s",
		current.Provider, current.Model, tracker.UserName,
	)

	sink := newMessageSink(bot, prompt.ChatID, lastMessageID, opts.MaxChars)

	// ---------------------------------------------------------------------
	// STREAM LOOP
	// ---------------------------------------------------------------------

	var messageText string
	responseID := ""
	lastEdit := time.Now()
	firstEdit := true

	// finish renders the final answer, builds the footer and returns.
	finish := func(stopped bool, err error) (Result, error) {
		result := Result{
			Provider:   current.Provider,
			Model:      current.Model,
			ResponseID: responseID,
			Text:       messageText,
			Failovers:  failovers,
			Stopped:    stopped,
			Latency:    time.Since(requestStart),
			Messages:   sink.Messages(),
		}

		text := messageText
		if strings.TrimSpace(text) != "" {
			text = strings.TrimRight(text, " \t\n")
			if opts.ShowFooter {
				text += "\n\n" + footer(current, failovers)
			}
		} else if stopped {
			text = "🛑 Stopped."
		}

		sink.Finish(text, opts.Markdown)

		if opts.Buttons {
			sink.SetKeyboard(ui.AnswerActions())
		}

		tracker.SetLastModel(current.Model)

		return result, err
	}

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
			// Some providers close an empty stream on overload. Treating that
			// as a failure lets the next candidate answer instead of leaving
			// the user with a blank message.
			if strings.TrimSpace(messageText) == "" {
				chain.RecordFailure(current.Provider, errors.New("empty completion"))

				if next, nextCandidate, nextIdx, ok := openNext(ctx, chain, candidates, idx+1, req); ok {
					_ = currentStream.Close()
					currentStream, current, idx = next, nextCandidate, nextIdx
					failovers++
					tracker.SetStream(currentStream, cancel)

					log.Printf("⚡ Empty answer, failing over to provider=%s model=%s", current.Provider, current.Model)

					continue
				}
			}

			log.Printf(
				"Stream finished | provider=%s | model=%s | response=%s",
				current.Provider, current.Model, responseID,
			)

			chain.RecordSuccessLatency(current.Provider, current.Model, time.Since(requestStart))

			tracker.AddMessage(openai.ChatMessageRoleUser, userText)
			tracker.AddMessage(openai.ChatMessageRoleAssistant, messageText)

			return finish(false, nil)
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
				}

				return finish(true, nil)
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

			if next, nextCandidate, nextIdx, ok := openNext(ctx, chain, candidates, idx+1, req); ok {
				_ = currentStream.Close()

				currentStream, current, idx = next, nextCandidate, nextIdx
				failovers++
				tracker.SetStream(currentStream, cancel)

				log.Printf("⚡ Failover to provider=%s model=%s", current.Provider, current.Model)

				continue
			}

			// ---------------------------------------------------------
			// KEEP PARTIAL OUTPUT
			// ---------------------------------------------------------

			if strings.TrimSpace(messageText) != "" {
				tracker.AddMessage(openai.ChatMessageRoleUser, userText)
				tracker.AddMessage(openai.ChatMessageRoleAssistant, messageText)

				return finish(false, nil)
			}

			tracker.SetLastModel(current.Model)
			sink.Finish(errorMessage, false)

			return Result{Provider: current.Provider, Model: current.Model, Failovers: failovers, Messages: sink.Messages()}, recvErr
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

		if !opts.Streaming {
			// The user asked for a single final message: do not spend edits
			// on partial text.
			continue
		}

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
) (*openai.ChatCompletionStream, provider.Candidate, int, bool) {
	for i := start; i < len(candidates); i++ {
		candidate := candidates[i]

		stream, err := candidate.Stream(ctx, req)
		if err == nil {
			chain.RecordSuccess(candidate.Provider)

			return stream, candidate, i, true
		}

		chain.RecordFailure(candidate.Provider, err)
		log.Printf(
			"Failover attempt failed | provider=%s model=%s error=%v",
			candidate.Provider, candidate.Model, err,
		)
	}

	return nil, provider.Candidate{}, 0, false
}

// footer names the model that answered and mentions a failover when one
// happened, so a slow or degraded backend is never invisible.
func footer(candidate provider.Candidate, failovers int) string {
	line := fmt.Sprintf("⚡ %s · %s", candidate.Provider, candidate.Model)

	if failovers > 0 {
		line += fmt.Sprintf("\n🔀 switched after %d failed attempt(s)", failovers)
	}

	return line
}

// GenerateRace executes a concurrent model race across 2-3 suitable candidates
// and renders the winning (or synthesised) answer into ChatID.
func GenerateRace(
	parentCtx context.Context,
	bot *tgbotapi.BotAPI,
	chain *provider.Chain,
	prompt Prompt,
	cfg *config.Config,
	tracker *user.UsageTracker,
	opts Options,
	synthesize bool,
) (Result, error) {
	if cfg == nil {
		return Result{}, errors.New("no configuration provided")
	}
	if chain == nil {
		return Result{}, errors.New("no provider chain configured")
	}

	tracker.ChatMu.Lock()
	defer tracker.ChatMu.Unlock()

	var profileVal provider.UsageProfile
	if opts.Profile != nil {
		profileVal = *opts.Profile
	}

	taskOverride := router.TaskFast
	if synthesize {
		taskOverride = router.TaskReasoning
	}

	candidates, _ := router.Route(chain, router.RouteRequest{
		Prompt:       prompt.Text,
		TaskOverride: taskOverride,
		Preference:   provider.Preference{Auto: true},
		Profile:      profileVal,
	})
	if len(candidates) == 0 {
		return Result{}, errors.New("no usable models in the provider chain")
	}

	placeholder := tgbotapi.NewMessage(prompt.ChatID, "🏎 Racing models...")
	if opts.Buttons {
		placeholder.ReplyMarkup = ui.StopButton()
	}

	sentMsg, err := bot.Send(placeholder)
	if err != nil {
		return Result{}, fmt.Errorf("failed to send race placeholder: %w", err)
	}

	stopTyping := typingLoop(bot, prompt.ChatID)
	defer stopTyping()

	tracker.CheckHistory(cfg.MaxHistorySize, cfg.MaxHistoryTime)
	tracker.Touch()

	currentMessage, userText := buildUserMessage(bot, prompt, cfg)

	sysContent := tracker.GetSystemPrompt()
	if extra := strings.TrimSpace(opts.SystemSupplement); extra != "" {
		sysContent = strings.TrimSpace(sysContent + "\n\n" + extra)
	}

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: sysContent},
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
	}

	ctx, cancel := context.WithTimeout(parentCtx, cfg.RequestTimeout)
	defer cancel()
	tracker.SetStream(nil, cancel)
	defer tracker.ClearStream()

	raceRes, err := router.Race(ctx, chain, candidates, req, router.RaceOptions{
		MaxModels:  router.DefaultRaceConcurrency,
		Timeout:    cfg.RequestTimeout,
		Synthesize: synthesize,
	})
	if err != nil {
		errorMessage := lang.Translate("errorText", cfg.Lang)
		if errorMessage == "" {
			errorMessage = "❌ Could not answer right now, please try again."
		}
		editMsg := tgbotapi.NewEditMessageText(prompt.ChatID, sentMsg.MessageID, errorMessage)
		_, _ = bot.Send(editMsg)
		return Result{}, err
	}

	tracker.AddMessage(openai.ChatMessageRoleUser, userText)
	tracker.AddMessage(openai.ChatMessageRoleAssistant, raceRes.Text)
	tracker.SetLastModel(raceRes.Model)

	text := strings.TrimRight(raceRes.Text, " \t\n")
	if opts.ShowFooter {
		modeTag := "🏎 race winner"
		if raceRes.Synthesized {
			modeTag = "⚖️ race synthesis"
		}
		text += fmt.Sprintf("\n\n⚡ %s · %s (%s, %s)", raceRes.Provider, raceRes.Model, modeTag, raceRes.Latency.Round(time.Millisecond))
	}

	sink := newMessageSink(bot, prompt.ChatID, sentMsg.MessageID, opts.MaxChars)
	sink.Finish(text, opts.Markdown)
	if opts.Buttons {
		sink.SetKeyboard(ui.AnswerActions())
	}

	return Result{
		Provider: raceRes.Provider,
		Model:    raceRes.Model,
		Text:     raceRes.Text,
		Latency:  raceRes.Latency,
		Messages: sink.Messages(),
	}, nil
}
