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
	} `json:"pricing"`
}

type APIResponse struct {
	Data []Model `json:"data"`
}

func GetFreeModels() (string, error) {
	manager, err := config.NewManager("./config.yaml")
	if err != nil {
		return "", fmt.Errorf("error initializing config manager: %v", err)
	}

	conf := manager.GetConfig()

	resp, err := http.Get(conf.OpenAIBaseURL + "/models")
	if err != nil {
		return "", fmt.Errorf("error get models: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error read response: %v", err)
	}

	var apiResponse APIResponse

	err = json.Unmarshal(body, &apiResponse)
	if err != nil {
		return "", fmt.Errorf("error parse json: %v", err)
	}

	var result strings.Builder

	for _, model := range apiResponse.Data {
		if model.Pricing.Prompt == "0" {
			result.WriteString(fmt.Sprintf("➡ `%s`\n", model.ID))
		}
	}

	return result.String(), nil
}

func HandleChatGPTStreamResponse(
	bot *tgbotapi.BotAPI,
	client *openai.Client,
	message *tgbotapi.Message,
	config *config.Config,
	user *user.UsageTracker,
) string {

	// ---------------------------------------------------------
	// ONE ACTIVE GENERATION PER USER
	// ---------------------------------------------------------

	user.ChatMu.Lock()
	defer user.ChatMu.Unlock()

	ctx := context.Background()

	user.CheckHistory(
		config.MaxHistorySize,
		config.MaxHistoryTime,
	)

	user.LastMessageTime = time.Now()

	// ---------------------------------------------------------
	// LOAD TRANSLATIONS
	// ---------------------------------------------------------

	err := lang.LoadTranslations("./lang/")
	if err != nil {
		log.Printf("Error loading translations: %v", err)
		return ""
	}

	manager, err := configs.NewManager("./config.yaml")
	if err != nil {
		log.Printf("Error initializing config manager: %v", err)
		return ""
	}

	conf := manager.GetConfig()

	loadMessage := lang.Translate("loadText", conf.Lang)
	errorMessage := lang.Translate("errorText", conf.Lang)

	// ---------------------------------------------------------
	// SEND INITIAL MESSAGE
	// ---------------------------------------------------------

	processingMsg := tgbotapi.NewMessage(
		message.Chat.ID,
		loadMessage,
	)

	sentMsg, err := bot.Send(processingMsg)
	if err != nil {
		log.Printf(
			"Failed to send processing message: %v",
			err,
		)
		return ""
	}

	lastMessageID := sentMsg.MessageID

	// ---------------------------------------------------------
	// LOADING ANIMATION
	// ---------------------------------------------------------

	stopAnimation := make(chan bool)

	go func() {

		dots := []string{
			"",
			".",
			"..",
			"...",
		}

		i := 0

		ticker := time.NewTicker(
			1500 * time.Millisecond,
		)

		defer ticker.Stop()

		for {

			select {

			case <-stopAnimation:
				return

			case <-ticker.C:

				text := fmt.Sprintf(
					"%s%s",
					loadMessage,
					dots[i],
				)

				editMsg := tgbotapi.NewEditMessageText(
					message.Chat.ID,
					lastMessageID,
					text,
				)

				if _, err := bot.Send(editMsg); err != nil {

					log.Printf(
						"Failed to update loading message: %v",
						err,
					)
				}

				i = (i + 1) % len(dots)
			}
		}
	}()

	// ---------------------------------------------------------
	// BUILD CONVERSATION
	// ---------------------------------------------------------

	messages := []openai.ChatCompletionMessage{
		{
			Role:    openai.ChatMessageRoleSystem,
			Content: user.SystemPrompt,
		},
	}

	for _, msg := range user.GetMessages() {

		messages = append(
			messages,
			openai.ChatCompletionMessage{
				Role:    msg.Role,
				Content: msg.Content,
			},
		)
	}

	// ---------------------------------------------------------
	// CURRENT MESSAGE
	// ---------------------------------------------------------

	if config.Vision == "true" {

		messages = append(
			messages,
			addVisionMessage(
				bot,
				message,
				config,
			),
		)

	} else {

		messages = append(
			messages,
			openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: message.Text,
			},
		)
	}

	// ---------------------------------------------------------
	// OPENROUTER REQUEST
	// ---------------------------------------------------------

	req := openai.ChatCompletionRequest{
		Model: config.Model.ModelName,

		FrequencyPenalty: float32(
			config.Model.FrequencyPenalty,
		),

		PresencePenalty: float32(
			config.Model.PresencePenalty,
		),

		Temperature: float32(
			config.Model.Temperature,
		),

		TopP: float32(
			config.Model.TopP,
		),

		MaxTokens: config.MaxTokens,

		Messages: messages,

		Stream: true,
	}

	// ---------------------------------------------------------
	// CREATE STREAM
	// ---------------------------------------------------------

	stream, err := client.CreateChatCompletionStream(
		ctx,
		req,
	)

	if err != nil {

		log.Printf(
			"ChatCompletionStream error: %v",
			err,
		)

		select {
		case stopAnimation <- true:
		default:
		}

		errorMsg := tgbotapi.NewEditMessageText(
			message.Chat.ID,
			lastMessageID,
			errorMessage,
		)

		if _, editErr := bot.Send(errorMsg); editErr != nil {

			log.Printf(
				"Failed to send error message: %v",
				editErr,
			)
		}

		return ""
	}

	defer stream.Close()

	user.CurrentStream = stream

	// Stop loading animation
	select {
	case stopAnimation <- true:
	default:
	}

	// ---------------------------------------------------------
	// STREAMING
	// ---------------------------------------------------------

	var messageText string

	responseID := ""

	// OpenRouter receives chunks immediately.
	// Telegram is updated every 700ms.
	lastEdit := time.Now()

	const editInterval = 700 * time.Millisecond

	log.Printf(
		"User: %s Stream response.",
		user.UserName,
	)

	// ---------------------------------------------------------
	// STREAM LOOP
	// ---------------------------------------------------------

	for {

		response, err := stream.Recv()

		// -----------------------------------------------------
		// RESPONSE ID
		// -----------------------------------------------------

		if responseID == "" &&
			response.ID != "" {

			responseID = response.ID
		}

		// -----------------------------------------------------
		// STREAM FINISHED
		// -----------------------------------------------------

		if errors.Is(err, io.EOF) {

			log.Printf(
				"Stream finished, response ID: %s",
				responseID,
			)

			// Save user message
			user.AddMessage(
				openai.ChatMessageRoleUser,
				message.Text,
			)

			// Save AI response
			user.AddMessage(
				openai.ChatMessageRoleAssistant,
				messageText,
			)

			// Final Telegram update
			if strings.TrimSpace(messageText) != "" {

				editMsg := tgbotapi.NewEditMessageText(
					message.Chat.ID,
					lastMessageID,
					messageText,
				)

				editMsg.ParseMode =
					tgbotapi.ModeMarkdown

				if _, err := bot.Send(editMsg); err != nil {

					log.Printf(
						"Failed to edit final message: %v",
						err,
					)
				}
			} else {

				log.Printf(
					"Final response was empty",
				)
			}

			user.CurrentStream = nil

			return responseID
		}

		// -----------------------------------------------------
		// STREAM ERROR
		// -----------------------------------------------------

		if err != nil {

			log.Printf(
				"Stream error: %v",
				err,
			)

			user.CurrentStream = nil

			if strings.TrimSpace(messageText) == "" {

				errorMsg :=
					tgbotapi.NewEditMessageText(
						message.Chat.ID,
						lastMessageID,
						errorMessage,
					)

				if _, editErr := bot.Send(errorMsg); editErr != nil {

					log.Printf(
						"Failed to send stream error: %v",
						editErr,
					)
				}
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
		// ADD STREAM CONTENT
		// -----------------------------------------------------

		content :=
			response.Choices[0].Delta.Content

		if content != "" {
			messageText += content
		}

		// Don't edit empty messages
		if strings.TrimSpace(messageText) == "" {
			continue
		}

		// -----------------------------------------------------
		// TELEGRAM STREAM THROTTLE
		// -----------------------------------------------------

		if time.Since(lastEdit) >= editInterval {

			editMsg :=
				tgbotapi.NewEditMessageText(
					message.Chat.ID,
					lastMessageID,
					messageText,
				)

			editMsg.ParseMode =
				tgbotapi.ModeMarkdown

			if _, err := bot.Send(editMsg); err != nil {

				log.Printf(
					"Failed to edit streaming message: %v",
					err,
				)

			} else {

				lastEdit = time.Now()
			}
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

		// Use largest photo
		photoSize :=
			message.Photo[len(message.Photo)-1]

		fileID := photoSize.FileID

		// Download photo
		file, err := bot.GetFile(
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
				Role:    openai.ChatMessageRoleUser,
				Content: message.Text,
			}
		}

		// Telegram file URL
		fileURL :=
			file.Link(bot.Token)

		log.Printf(
			"Photo URL: %s",
			fileURL,
		)

		if message.Text == "" {
			message.Text =
				config.VisionPrompt
		}

		return openai.ChatCompletionMessage{

			Role:
				openai.ChatMessageRoleUser,

			MultiContent:
				[]openai.ChatMessagePart{

					{
						Type:
							openai.ChatMessagePartTypeText,

						Text:
							message.Text,
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

	return openai.ChatCompletionMessage{
		Role:
			openai.ChatMessageRoleUser,

		Content:
			message.Text,
	}
}
