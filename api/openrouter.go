package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"openrouter-bot/config"
	configs "openrouter-bot/config"
	"openrouter-bot/lang"
	"openrouter-bot/user"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sashabaranov/go-openai"
)

type Model struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Pricing     struct {
		Prompt string `json:"prompt"`
	}
}

type APIResponse struct {
	Data []Model `json:"data"`
}

// -------------------------------------------------------------
// GET FREE MODELS
// -------------------------------------------------------------

func GetFreeModels() (string, error) {

	manager, err := config.NewManager("./config.yaml")
	if err != nil {
		return "", fmt.Errorf(
			"error initializing config manager: %v",
			err,
		)
	}

	conf := manager.GetConfig()

	resp, err := http.Get(
		conf.OpenAIBaseURL + "/models",
	)
	if err != nil {
		return "", fmt.Errorf(
			"error get models: %v",
			err,
		)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf(
			"error read response: %v",
			err,
		)
	}

	var apiResponse APIResponse

	if err := json.Unmarshal(
		body,
		&apiResponse,
	); err != nil {
		return "", fmt.Errorf(
			"error parse json: %v",
			err,
		)
	}

	var result strings.Builder

	for _, model := range apiResponse.Data {

		if model.Pricing.Prompt == "0" {

			result.WriteString(
				fmt.Sprintf(
					"➡ `%s`\n",
					model.ID,
				),
			)
		}
	}

	return result.String(), nil
}

// -------------------------------------------------------------
// MAIN CHAT HANDLER
// -------------------------------------------------------------

func HandleChatGPTStreamResponse(
	bot *tgbotapi.BotAPI,
	client *openai.Client,
	geminiClient *openai.Client,
	message *tgbotapi.Message,
	config *config.Config,
	user *user.UsageTracker,
) string {

	// ---------------------------------------------------------
	// ONE ACTIVE GENERATION PER USER
	// ---------------------------------------------------------

	user.ChatMu.Lock()
	defer user.ChatMu.Unlock()

	// ---------------------------------------------------------
	// REQUEST CONTEXT
	// ---------------------------------------------------------

	ctx, cancel := context.WithTimeout(
		context.Background(),
		120*time.Second,
	)

	defer cancel()

	user.CheckHistory(
		config.MaxHistorySize,
		config.MaxHistoryTime,
	)

	user.LastMessageTime = time.Now()

	// ---------------------------------------------------------
	// LOAD TRANSLATIONS
	// ---------------------------------------------------------

	if err := lang.LoadTranslations("./lang/"); err != nil {

		log.Printf(
			"Error loading translations: %v",
			err,
		)

		return ""
	}

	manager, err := configs.NewManager(
		"./config.yaml",
	)

	if err != nil {

		log.Printf(
			"Error initializing config manager: %v",
			err,
		)

		return ""
	}

	conf := manager.GetConfig()

	loadMessage :=
		lang.Translate(
			"loadText",
			conf.Lang,
		)

	errorMessage :=
		lang.Translate(
			"errorText",
			conf.Lang,
		)

	// ---------------------------------------------------------
	// SEND INITIAL MESSAGE
	// ---------------------------------------------------------

	processingMsg :=
		tgbotapi.NewMessage(
			message.Chat.ID,
			loadMessage,
		)

	sentMsg, err :=
		bot.Send(processingMsg)

	if err != nil {

		log.Printf(
			"Failed to send processing message: %v",
			err,
		)

		return ""
	}

	lastMessageID :=
		sentMsg.MessageID

	// ---------------------------------------------------------
	// LOADING ANIMATION
	// ---------------------------------------------------------

	stopAnimation :=
		make(chan bool, 1)

	go func() {

		dots := []string{
			"",
			".",
			"..",
			"...",
		}

		i := 0

		ticker :=
			time.NewTicker(
				1200 * time.Millisecond,
			)

		defer ticker.Stop()

		for {

			select {

			case <-stopAnimation:
				return

			case <-ticker.C:

				text :=
					loadMessage + dots[i]

				editMsg :=
					tgbotapi.NewEditMessageText(
						message.Chat.ID,
						lastMessageID,
						text,
					)

				if _, err :=
					bot.Send(editMsg); err != nil {

					if !strings.Contains(
						err.Error(),
						"message is not modified",
					) {

						log.Printf(
							"Loading edit error: %v",
							err,
						)
					}
				}

				i =
					(i + 1) %
						len(dots)
			}
		}
	}()

	// ---------------------------------------------------------
	// BUILD CONVERSATION
	// ---------------------------------------------------------

	messages :=
		[]openai.ChatCompletionMessage{
			{
				Role:
					openai.ChatMessageRoleSystem,

				Content:
					user.SystemPrompt,
			},
		}

	for _, msg :=
		range user.GetMessages() {

		messages =
			append(
				messages,
				openai.ChatCompletionMessage{
					Role:
						msg.Role,

					Content:
						msg.Content,
				},
			)
	}

	// ---------------------------------------------------------
	// CURRENT USER MESSAGE
	// ---------------------------------------------------------

	if config.Vision == "true" {

		messages =
			append(
				messages,
				addVisionMessage(
					bot,
					message,
					config,
				),
			)

	} else {

		messages =
			append(
				messages,
				openai.ChatCompletionMessage{
					Role:
						openai.ChatMessageRoleUser,

					Content:
						message.Text,
				},
			)
	}

	// ---------------------------------------------------------
	// OPENROUTER REQUEST
	// ---------------------------------------------------------

	req :=
		openai.ChatCompletionRequest{

			Model:
				config.Model.ModelName,

			FrequencyPenalty:
				float32(
					config.Model.FrequencyPenalty,
				),

			PresencePenalty:
				float32(
					config.Model.PresencePenalty,
				),

			Temperature:
				float32(
					config.Model.Temperature,
				),

			TopP:
				float32(
					config.Model.TopP,
				),

			MaxTokens:
				config.MaxTokens,

			Messages:
				messages,

			Stream:
				true,
		}

	// ---------------------------------------------------------
	// TRY OPENROUTER
	// ---------------------------------------------------------

	stream, err :=
		client.CreateChatCompletionStream(
			ctx,
			req,
		)

	usingGemini := false

	// ---------------------------------------------------------
	// OPENROUTER INITIAL FAILURE
	// ---------------------------------------------------------

	if err != nil {

		log.Printf(
			"OpenRouter stream creation error: %v",
			err,
		)

		if geminiClient != nil &&
			isProviderRateLimit(err) {

			log.Printf(
				"⚡ OpenRouter unavailable. Switching to Gemini.",
			)

			stream, err =
				createGeminiStream(
					ctx,
					geminiClient,
					messages,
				)

			if err == nil {

				usingGemini = true

				log.Printf(
					"⚡ Gemini fallback stream started.",
				)
			}
		}
	}

	// ---------------------------------------------------------
	// BOTH FAILED
	// ---------------------------------------------------------

	if err != nil {

		stopLoading(stopAnimation)

		log.Printf(
			"All AI providers failed: %v",
			err,
		)

		editMsg :=
			tgbotapi.NewEditMessageText(
				message.Chat.ID,
				lastMessageID,
				errorMessage,
			)

		if _, editErr :=
			bot.Send(editMsg); editErr != nil {

			log.Printf(
				"Failed to send error message: %v",
				editErr,
			)
		}

		return ""
	}

	defer stream.Close()

	user.CurrentStream =
		stream

	stopLoading(stopAnimation)

	// ---------------------------------------------------------
	// STREAM VARIABLES
	// ---------------------------------------------------------

	var messageText string

	responseID := ""

	lastEdit :=
		time.Now()

	// Telegram API calls are relatively expensive.
	// 350ms gives a fast perceived response without
	// hammering Telegram.
	const editInterval =
		350 * time.Millisecond

	log.Printf(
		"⚡ Stream started | provider=%s | user=%s",
		providerName(usingGemini),
		user.UserName,
	)

	// ---------------------------------------------------------
	// STREAM LOOP
	// ---------------------------------------------------------

	for {

		response, recvErr :=
			stream.Recv()

		// -----------------------------------------------------
		// RESPONSE ID
		// -----------------------------------------------------

		if responseID == "" &&
			response.ID != "" {

			responseID =
				response.ID
		}

		// -----------------------------------------------------
		// NORMAL STREAM END
		// -----------------------------------------------------

		if errors.Is(
			recvErr,
			io.EOF,
		) {

			log.Printf(
				"Stream finished | provider=%s | response=%s",
				providerName(usingGemini),
				responseID,
			)

			user.CurrentStream =
				nil

			// Save conversation.
			user.AddMessage(
				openai.ChatMessageRoleUser,
				message.Text,
			)

			user.AddMessage(
				openai.ChatMessageRoleAssistant,
				messageText,
			)

			// Final response.
			if strings.TrimSpace(
				messageText,
			) != "" {

				editFinalMessage(
					bot,
					message.Chat.ID,
					lastMessageID,
					messageText,
				)
			}

			return responseID
		}

		// -----------------------------------------------------
		// STREAM ERROR
		// -----------------------------------------------------

		if recvErr != nil {

			log.Printf(
				"Stream error | provider=%s | error=%v",
				providerName(usingGemini),
				recvErr,
			)

			// -------------------------------------------------
			// FALLBACK TO GEMINI
			// -------------------------------------------------

			if !usingGemini &&
				geminiClient != nil {

				log.Printf(
					"⚡ OpenRouter stream failed. Retrying with Gemini.",
				)

				// Close broken OpenRouter stream.
				stream.Close()

				geminiStream, geminiErr :=
					createGeminiStream(
						ctx,
						geminiClient,
						messages,
					)

				if geminiErr == nil {

					stream =
						geminiStream

					usingGemini =
						true

					user.CurrentStream =
						stream

					log.Printf(
						"⚡ Gemini retry stream started.",
					)

					// Continue streaming.
					continue
				}

				log.Printf(
					"Gemini retry failed: %v",
					geminiErr,
				)
			}

			user.CurrentStream =
				nil

			// If we already received content,
			// keep that content instead of replacing
			// it with an error.
			if strings.TrimSpace(
				messageText,
			) != "" {

				editFinalMessage(
					bot,
					message.Chat.ID,
					lastMessageID,
					messageText,
				)

				user.AddMessage(
					openai.ChatMessageRoleUser,
					message.Text,
				)

				user.AddMessage(
					openai.ChatMessageRoleAssistant,
					messageText,
				)

				return responseID
			}

			editMsg :=
				tgbotapi.NewEditMessageText(
					message.Chat.ID,
					lastMessageID,
					errorMessage,
				)

			if _, editErr :=
				bot.Send(editMsg); editErr != nil {

				log.Printf(
					"Failed to send stream error: %v",
					editErr,
				)
			}

			return responseID
		}

		// -----------------------------------------------------
		// EMPTY CHOICES
		// -----------------------------------------------------

		if len(response.Choices) == 0 {
			continue
		}

		// -----------------------------------------------------
		// GET TOKEN
		// -----------------------------------------------------

		content :=
			response.Choices[0].
				Delta.Content

		if content == "" {
			continue
		}

		messageText +=
			content

		// -----------------------------------------------------
		// FIRST TOKEN
		// -----------------------------------------------------

		// Send the first visible token immediately.
		//
		// This is important for perceived ChatGPT-like
		// responsiveness.

		if strings.TrimSpace(
			messageText,
		) != "" {

			if lastEdit.IsZero() ||
				len(messageText) <= len(content) {

				editStreamingMessage(
					bot,
					message.Chat.ID,
					lastMessageID,
					messageText,
				)

				lastEdit =
					time.Now()

				continue
			}
		}

		// -----------------------------------------------------
		// NORMAL STREAMING UPDATE
		// -----------------------------------------------------

		if time.Since(
			lastEdit,
		) >= editInterval {

			if editStreamingMessage(
				bot,
				message.Chat.ID,
				lastMessageID,
				messageText,
			) {

				lastEdit =
					time.Now()
			}
		}
	}
}

// -------------------------------------------------------------
// GEMINI STREAM
// -------------------------------------------------------------

func createGeminiStream(
	parentCtx context.Context,
	client *openai.Client,
	messages []openai.ChatCompletionMessage,
) (*openai.ChatCompletionStream, error) {

	// Shorter timeout specifically for Gemini fallback.
	ctx, cancel :=
		context.WithTimeout(
			parentCtx,
			60*time.Second,
		)

	// The stream owns the HTTP request, so we cannot
	// immediately cancel this context.
	//
	// The timeout will automatically terminate the
	// request after 60 seconds.
	_ = cancel

	req :=
		openai.ChatCompletionRequest{

			Model:
				"gemini-3.8-flash",

			Messages:
				messages,

			Stream:
				true,

			// Keep fallback responses reasonably short.
			MaxTokens:
				1000,

			Temperature:
				0.7,

			TopP:
				0.8,
		}

	return client.CreateChatCompletionStream(
		ctx,
		req,
	)
}

// -------------------------------------------------------------
// PROVIDER NAME
// -------------------------------------------------------------

func providerName(
	gemini bool,
) string {

	if gemini {
		return "Gemini"
	}

	return "OpenRouter"
}

// -------------------------------------------------------------
// RATE LIMIT DETECTION
// -------------------------------------------------------------

func isProviderRateLimit(
	err error,
) bool {

	if err == nil {
		return false
	}

	errText :=
		strings.ToLower(
			err.Error(),
		)

	return strings.Contains(
		errText,
		"429",
	) ||
		strings.Contains(
			errText,
			"too many requests",
		) ||
		strings.Contains(
			errText,
			"rate limit",
		) ||
		strings.Contains(
			errText,
			"rate_limit",
		)
}

// -------------------------------------------------------------
// STOP LOADING ANIMATION
// -------------------------------------------------------------

func stopLoading(
	stopAnimation chan bool,
) {

	select {

	case stopAnimation <- true:

	default:
	}
}

// -------------------------------------------------------------
// FAST STREAMING EDIT
// -------------------------------------------------------------

func editStreamingMessage(
	bot *tgbotapi.BotAPI,
	chatID int64,
	messageID int,
	text string,
) bool {

	if strings.TrimSpace(text) == "" {
		return false
	}

	// IMPORTANT:
	//
	// Do NOT use Markdown while streaming.
	//
	// AI responses often contain incomplete Markdown
	// such as:
	//
	// **hello
	//
	// which can cause Telegram parse errors.
	//
	// Plain text makes streaming much more reliable.

	editMsg :=
		tgbotapi.NewEditMessageText(
			chatID,
			messageID,
			text,
		)

	if _, err :=
		bot.Send(editMsg); err != nil {

		if strings.Contains(
			err.Error(),
			"message is not modified",
		) {
			return false
		}

		log.Printf(
			"Streaming Telegram edit error: %v",
			err,
		)

		return false
	}

	return true
}

// -------------------------------------------------------------
// FINAL MESSAGE
// -------------------------------------------------------------

func editFinalMessage(
	bot *tgbotapi.BotAPI,
	chatID int64,
	messageID int,
	text string,
) {

	if strings.TrimSpace(text) == "" {
		return
	}

	editMsg :=
		tgbotapi.NewEditMessageText(
			chatID,
			messageID,
			text,
		)

	// Use Markdown only for the final message.
	editMsg.ParseMode =
		tgbotapi.ModeMarkdown

	if _, err :=
		bot.Send(editMsg); err != nil {

		// If Markdown parsing fails, send plain text.
		log.Printf(
			"Markdown final edit failed: %v",
			err,
		)

		plainMsg :=
			tgbotapi.NewEditMessageText(
				chatID,
				messageID,
				text,
			)

		if _, plainErr :=
			bot.Send(plainMsg); plainErr != nil {

			log.Printf(
				"Plain final edit failed: %v",
				plainErr,
			)
		}
	}
}

// -------------------------------------------------------------
// VISION MESSAGE
// -------------------------------------------------------------

func addVisionMessage(
	bot *tgbotapi.BotAPI,
	message *tgbotapi.Message,
	config *config.Config,
) openai.ChatCompletionMessage {

	if len(message.Photo) > 0 {

		// -----------------------------------------------------
		// LARGEST PHOTO
		// -----------------------------------------------------

		photoSize :=
			message.Photo[
				len(message.Photo)-1,
			]

		fileID :=
			photoSize.FileID

		// -----------------------------------------------------
		// TELEGRAM FILE
		// -----------------------------------------------------

		file, err :=
			bot.GetFile(
				tgbotapi.FileConfig{
					FileID: fileID,
				},
			)

		if err != nil {

			log.Printf(
				"Error getting file: %v",
				err,
			)

			return openai.ChatCompletionMessage{
				Role:
					openai.ChatMessageRoleUser,

				Content:
					message.Text,
			}
		}

		fileURL :=
			file.Link(
				bot.Token,
			)

		// -----------------------------------------------------
		// VISION PROMPT
		// -----------------------------------------------------

		if strings.TrimSpace(
			message.Text,
		) == "" {

			message.Text =
				config.VisionPrompt
		}

		if strings.TrimSpace(
			message.Text,
		) == "" {

			message.Text =
				"Describe this image."
		}

		// -----------------------------------------------------
		// MULTIMODAL MESSAGE
		// -----------------------------------------------------

		return openai.ChatCompletionMessage{

			Role:
				openai.ChatMessageRoleUser,

			MultiContent:
				[]openai.ChatMessagePart{

					{
						Type:
							openai.ChatMessagePartTypeText,

						Text:
							strings.TrimSpace(
								message.Text,
							),
					},

					{
						Type:
							openai.ChatMessagePartTypeImageURL,

						ImageURL:
							&openai.ChatMessageImageURL{

								URL:
									fileURL,

								Detail:
									openai.ImageURLDetail(
										config.VisionDetails,
									),
							},
					},
				},
		}
	}

	// ---------------------------------------------------------
	// NORMAL TEXT
	// ---------------------------------------------------------

	return openai.ChatCompletionMessage{

		Role:
			openai.ChatMessageRoleUser,

		Content:
			message.Text,
	}
}
