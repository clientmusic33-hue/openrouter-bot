package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"openrouter-bot/api"
	"openrouter-bot/config"
	"openrouter-bot/grouptranslate"
	"openrouter-bot/lang"
	"openrouter-bot/translator"
	"openrouter-bot/user"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sashabaranov/go-openai"
)

func main() {
	// Render Web Service health server.
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}

	go func() {
		mux := http.NewServeMux()

		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OpenRouter Telegram Bot is running"))
		})

		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
		})

		log.Printf("HTTP server listening on 0.0.0.0:%s", port)

		if err := http.ListenAndServe("0.0.0.0:"+port, mux); err != nil {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Load translations.
	err := lang.LoadTranslations("./lang/")
	if err != nil {
		log.Fatalf("Error loading translations: %v", err)
	}

	// Initialize configuration.
	manager, err := config.NewManager("./config.yaml")
	if err != nil {
		log.Fatalf("Error initializing config manager: %v", err)
	}

	conf := manager.GetConfig()

	// Initialize Telegram bot.
	bot, err := tgbotapi.NewBotAPI(conf.TelegramBotToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = false

	// Delete webhook so long polling can work.
	_, err = bot.Request(tgbotapi.DeleteWebhookConfig{})
	if err != nil {
		log.Fatalf("Failed to delete webhook: %v", err)
	}

	// Telegram long polling.
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	// Initialize group translation manager.
	translationManager := grouptranslate.NewManager(
		"data/translations.json",
	)

	// Set Telegram bot commands.
	commands := []tgbotapi.BotCommand{
		{
			Command:     "start",
			Description: lang.Translate("description.start", conf.Lang),
		},
		{
			Command:     "help",
			Description: lang.Translate("description.help", conf.Lang),
		},
		{
			Command:     "get_models",
			Description: lang.Translate("description.getModels", conf.Lang),
		},
		{
			Command:     "set_model",
			Description: lang.Translate("description.setModel", conf.Lang),
		},
		{
			Command:     "reset",
			Description: lang.Translate("description.reset", conf.Lang),
		},
		{
			Command:     "stats",
			Description: lang.Translate("description.stats", conf.Lang),
		},
		{
			Command:     "stop",
			Description: lang.Translate("description.stop", conf.Lang),
		},
		{
			Command:     "about",
			Description: "About this bot",
		},
		{
			Command:     "tr",
			Description: "Translate a replied message",
		},
		{
			Command:     "translate",
			Description: "Manage group auto translation",
		},
	}

	_, err = bot.Request(tgbotapi.NewSetMyCommands(commands...))
	if err != nil {
		log.Fatalf("Failed to set bot commands: %v", err)
	}

	// OpenRouter/OpenAI client.
	clientOptions := openai.DefaultConfig(conf.OpenAIApiKey)
	clientOptions.BaseURL = conf.OpenAIBaseURL
	client := openai.NewClientWithConfig(clientOptions)

	// User manager.
	userManager := user.NewUserManager("logs")

	// Process Telegram updates.
	for update := range updates {
		if update.Message == nil {
			continue
		}

		// Ignore messages sent by bots.
		if update.Message.From != nil && update.Message.From.IsBot {
			continue
		}

		userStats := userManager.GetUser(
			update.SentFrom().ID,
			update.SentFrom().UserName,
			conf,
		)

		// ---------------------------------------------------------
		// COMMANDS
		// ---------------------------------------------------------
		if update.Message.IsCommand() {
			switch update.Message.Command() {

			// /start
			case "start":
				msgText :=
					lang.Translate("commands.start", conf.Lang) +
						lang.Translate("commands.help", conf.Lang) +
						lang.Translate("commands.start_end", conf.Lang)

				msg := tgbotapi.NewMessage(
					update.Message.Chat.ID,
					msgText,
				)

				msg.ParseMode = "HTML"

				if _, err := bot.Send(msg); err != nil {
					log.Println("Failed to send /start:", err)
				}

			// /help
			case "help":
				msg := tgbotapi.NewMessage(
					update.Message.Chat.ID,
					lang.Translate("commands.help", conf.Lang),
				)

				msg.ParseMode = "HTML"

				if _, err := bot.Send(msg); err != nil {
					log.Println("Failed to send /help:", err)
				}

			// /get_models
			case "get_models":
				models, err := api.GetFreeModels()

				if err != nil {
					log.Printf("Error getting models: %v", err)

					msg := tgbotapi.NewMessage(
						update.Message.Chat.ID,
						"❌ Failed to get available models. Please try again later.",
					)

					if _, err := bot.Send(msg); err != nil {
						log.Println("Failed to send models error:", err)
					}

					continue
				}

				text := lang.Translate(
					"commands.getModels",
					conf.Lang,
				) + models

				msg := tgbotapi.NewMessage(
					update.Message.Chat.ID,
					text,
				)

				msg.ParseMode = tgbotapi.ModeMarkdown

				if _, err := bot.Send(msg); err != nil {
					log.Println("Failed to send models:", err)
				}

			// /set_model
			case "set_model":
				args := update.Message.CommandArguments()
				argsArr := strings.Fields(args)

				msg := tgbotapi.NewMessage(
					update.Message.Chat.ID,
					conf.Model.ModelName,
				)

				msg.ParseMode = tgbotapi.ModeMarkdown

				switch {
				case args == "default":
					conf.Model.ModelName = conf.Model.ModelNameDefault

					msg.Text =
						lang.Translate("commands.setModel", conf.Lang) +
							" `" +
							conf.Model.ModelName +
							"`"

				case args == "":
					msg.Text = lang.Translate(
						"commands.noArgsModel",
						conf.Lang,
					)

				case len(argsArr) > 1:
					msg.Text = lang.Translate(
						"commands.noSpaceModel",
						conf.Lang,
					)

				default:
					conf.Model.ModelName = argsArr[0]

					msg.Text =
						lang.Translate("commands.setModel", conf.Lang) +
							" `" +
							conf.Model.ModelName +
							"`"
				}

				if _, err := bot.Send(msg); err != nil {
					log.Println("Failed to send /set_model:", err)
				}

			// /reset
			case "reset":
				args := update.Message.CommandArguments()

				msg := tgbotapi.NewMessage(
					update.Message.Chat.ID,
					"",
				)

				if args == "system" {
					userStats.SystemPrompt = conf.SystemPrompt

					msg.Text = lang.Translate(
						"commands.reset_system",
						conf.Lang,
					)

				} else if args != "" {
					userStats.SystemPrompt = args

					msg.Text =
						lang.Translate(
							"commands.reset_prompt",
							conf.Lang,
						) +
							args +
							"."

				} else {
					userStats.ClearHistory()

					msg.Text = lang.Translate(
						"commands.reset",
						conf.Lang,
					)
				}

				if _, err := bot.Send(msg); err != nil {
					log.Println("Failed to send /reset:", err)
				}

			// /stats
			case "stats":
				userStats.CheckHistory(
					conf.MaxHistorySize,
					conf.MaxHistoryTime,
				)

				countedUsage := strconv.FormatFloat(
					userStats.GetCurrentCost(conf.BudgetPeriod),
					'f',
					6,
					64,
				)

				todayUsage := strconv.FormatFloat(
					userStats.GetCurrentCost("daily"),
					'f',
					6,
					64,
				)

				monthUsage := strconv.FormatFloat(
					userStats.GetCurrentCost("monthly"),
					'f',
					6,
					64,
				)

				totalUsage := strconv.FormatFloat(
					userStats.GetCurrentCost("total"),
					'f',
					6,
					64,
				)

				messagesCount := strconv.Itoa(
					len(userStats.GetMessages()),
				)

				var statsMessage string

				if userStats.CanViewStats(conf) {
					statsMessage = fmt.Sprintf(
						lang.Translate(
							"commands.stats",
							conf.Lang,
						),
						countedUsage,
						todayUsage,
						monthUsage,
						totalUsage,
						messagesCount,
					)
				} else {
					statsMessage = fmt.Sprintf(
						lang.Translate(
							"commands.stats_min",
							conf.Lang,
						),
						messagesCount,
					)
				}

				msg := tgbotapi.NewMessage(
					update.Message.Chat.ID,
					statsMessage,
				)

				msg.ParseMode = "HTML"

				if _, err := bot.Send(msg); err != nil {
					log.Println("Failed to send /stats:", err)
				}

			// /tr
			case "tr":
				if update.Message.ReplyToMessage == nil {
					msg := tgbotapi.NewMessage(
						update.Message.Chat.ID,
						"❌ Reply to a message and use:\n\n/tr hi\n/tr en\n/tr ru",
					)

					if _, err := bot.Send(msg); err != nil {
						log.Println("Failed to send /tr help:", err)
					}

					continue
				}

				targetLanguage := strings.TrimSpace(
					update.Message.CommandArguments(),
				)

				if targetLanguage == "" {
					targetLanguage = "English"
				}

				sourceText := update.Message.ReplyToMessage.Text

				if strings.TrimSpace(sourceText) == "" {
					msg := tgbotapi.NewMessage(
						update.Message.Chat.ID,
						"❌ The replied message doesn't contain text.",
					)

					if _, err := bot.Send(msg); err != nil {
						log.Println("Failed to send /tr error:", err)
					}

					continue
				}

				translatedText, err := translator.Translate(
					context.Background(),
					client,
					sourceText,
					targetLanguage,
					conf.Model.ModelName,
				)

				if err != nil {
					log.Printf("Translation error: %v", err)

					msg := tgbotapi.NewMessage(
						update.Message.Chat.ID,
						"❌ Translation failed. Please try again.",
					)

					if _, err := bot.Send(msg); err != nil {
						log.Println("Failed to send translation error:", err)
					}

					continue
				}

				msg := tgbotapi.NewMessage(
					update.Message.Chat.ID,
					"🌐 <b>Translation</b>\n\n"+translatedText,
				)

				msg.ParseMode = "HTML"

				if _, err := bot.Send(msg); err != nil {
					log.Println("Failed to send translation:", err)
				}

			// /translate
			case "translate":
				// Auto translation only makes sense in groups.
				if update.Message.Chat.Type != "group" &&
					update.Message.Chat.Type != "supergroup" {

					msg := tgbotapi.NewMessage(
						update.Message.Chat.ID,
						"❌ This command can only be used in a group.",
					)

					if _, err := bot.Send(msg); err != nil {
						log.Println("Failed to send /translate error:", err)
					}

					continue
				}

				}

				args := strings.TrimSpace(
					update.Message.CommandArguments(),
				)

				argsLower := strings.ToLower(args)

				switch argsLower {

				case "on":
					err := translationManager.SetEnabled(
						update.Message.Chat.ID,
						true,
					)

					if err != nil {
						log.Printf("Failed to enable translation: %v", err)

						bot.Send(tgbotapi.NewMessage(
							update.Message.Chat.ID,
							"❌ Failed to save translation settings.",
						))

						continue
					}

					settings := translationManager.Get(
						update.Message.Chat.ID,
					)

					msg := tgbotapi.NewMessage(
						update.Message.Chat.ID,
						fmt.Sprintf(
							"🌐 <b>Auto Translation Enabled</b>\n\n"+
								"Target language: <b>%s</b>\n\n"+
								"New group messages will now be translated automatically.",
							settings.TargetLanguage,
						),
					)

					msg.ParseMode = "HTML"

					if _, err := bot.Send(msg); err != nil {
						log.Println("Failed to send translation status:", err)
					}

				case "off":
					err := translationManager.SetEnabled(
						update.Message.Chat.ID,
						false,
					)

					if err != nil {
						log.Printf("Failed to disable translation: %v", err)

						bot.Send(tgbotapi.NewMessage(
							update.Message.Chat.ID,
							"❌ Failed to save translation settings.",
						))

						continue
					}

					bot.Send(tgbotapi.NewMessage(
						update.Message.Chat.ID,
						"🌐 Auto translation disabled.",
					))

				case "status":
					settings := translationManager.Get(
						update.Message.Chat.ID,
					)

					status := "🔴 Disabled"

					if settings.Enabled {
						status = "🟢 Enabled"
					}

					msg := tgbotapi.NewMessage(
						update.Message.Chat.ID,
						fmt.Sprintf(
							"🌐 <b>Translation Status</b>\n\n"+
								"Status: %s\n"+
								"Language: <b>%s</b>\n"+
								"Mode: Automatic",
							status,
							settings.TargetLanguage,
						),
					)

					msg.ParseMode = "HTML"

					if _, err := bot.Send(msg); err != nil {
						log.Println("Failed to send translation status:", err)
					}

				default:
					if args == "" {
						msg := tgbotapi.NewMessage(
							update.Message.Chat.ID,
							"🌐 <b>Group Translation</b>\n\n"+
								"/translate on — Enable\n"+
								"/translate off — Disable\n"+
								"/translate hi — Set Hindi and enable\n"+
								"/translate en — Set English and enable\n"+
								"/translate status — Show settings",
						)

						msg.ParseMode = "HTML"

						if _, err := bot.Send(msg); err != nil {
							log.Println("Failed to send translation help:", err)
						}

						continue
					}

					// Set the target language.
					err := translationManager.SetLanguage(
						update.Message.Chat.ID,
						args,
					)

					if err != nil {
						log.Printf(
							"Failed to set translation language: %v",
							err,
						)

						bot.Send(tgbotapi.NewMessage(
							update.Message.Chat.ID,
							"❌ Failed to save translation language.",
						))

						continue
					}

					// Automatically enable translation.
					err = translationManager.SetEnabled(
						update.Message.Chat.ID,
						true,
					)

					if err != nil {
						log.Printf(
							"Failed to enable translation: %v",
							err,
						)

						bot.Send(tgbotapi.NewMessage(
							update.Message.Chat.ID,
							"❌ Language was saved, but translation could not be enabled.",
						))

						continue
					}

					settings := translationManager.Get(
						update.Message.Chat.ID,
					)

					msg := tgbotapi.NewMessage(
						update.Message.Chat.ID,
						fmt.Sprintf(
							"🌐 <b>Auto Translation Enabled</b>\n\n"+
								"Target language: <b>%s</b>\n\n"+
								"You don't need to specify the language again.",
							settings.TargetLanguage,
						),
					)

					msg.ParseMode = "HTML"

					if _, err := bot.Send(msg); err != nil {
						log.Println("Failed to send translation settings:", err)
					}
				}

			// /about
			case "about":
				msg := tgbotapi.NewMessage(
					update.Message.Chat.ID,
					"🤖 <b>OpenRouter AI Bot</b>\n\n"+
						"👨‍💻 <b>Created by:</b> @Hazel21_nut\n"+
						"⚡ <b>Powered by:</b> OpenRouter",
				)

				msg.ParseMode = "HTML"

				if _, err := bot.Send(msg); err != nil {
					log.Println("Failed to send /about:", err)
				}

			// /stop
			case "stop":
				if userStats.CurrentStream != nil {
					userStats.CurrentStream.Close()

					msg := tgbotapi.NewMessage(
						update.Message.Chat.ID,
						lang.Translate(
							"commands.stop",
							conf.Lang,
						),
					)

					if _, err := bot.Send(msg); err != nil {
						log.Println("Failed to send /stop:", err)
					}

				} else {
					msg := tgbotapi.NewMessage(
						update.Message.Chat.ID,
						lang.Translate(
							"commands.stop_err",
							conf.Lang,
						),
					)

					if _, err := bot.Send(msg); err != nil {
						log.Println("Failed to send /stop error:", err)
					}
				}
			}

			continue
		}

		// ---------------------------------------------------------
		// ---------------------------------------------------------
// AUTOMATIC GROUP TRANSLATION
// ---------------------------------------------------------

if update.Message.Chat.Type == "group" ||
	update.Message.Chat.Type == "supergroup" {

	settings := translationManager.Get(
		update.Message.Chat.ID,
	)

	log.Printf(
		"Group translation check: chat=%d enabled=%v language=%q text=%q",
		update.Message.Chat.ID,
		settings.Enabled,
		settings.TargetLanguage,
		update.Message.Text,
	)

	if settings.Enabled &&
		strings.TrimSpace(update.Message.Text) != "" {

		sourceText := strings.TrimSpace(
			update.Message.Text,
		)

		targetLanguage := strings.TrimSpace(
			settings.TargetLanguage,
		)

		chatID := update.Message.Chat.ID
		messageID := update.Message.MessageID

		go func(
			chatID int64,
			messageID int,
			text string,
			target string,
		) {

			log.Printf(
				"Starting automatic translation: chat=%d message=%d target=%q",
				chatID,
				messageID,
				target,
			)

			translatedText, err := translator.Translate(
				context.Background(),
				client,
				text,
				target,
				conf.Model.ModelName,
			)

			if err != nil {
				log.Printf(
					"Automatic translation FAILED: chat=%d message=%d error=%v",
					chatID,
					messageID,
					err,
				)
				return
			}

			log.Printf(
				"Automatic translation SUCCESS: chat=%d message=%d",
				chatID,
				messageID,
			)

			msg := tgbotapi.NewMessage(
				chatID,
				fmt.Sprintf(
					"🌐 <b>%s</b>\n\n%s",
					target,
					translatedText,
				),
			)

			msg.ParseMode = "HTML"

			// Reply directly to the original message.
			msg.ReplyToMessageID = messageID

			if _, err := bot.Send(msg); err != nil {
				log.Printf(
					"Failed to send automatic translation: chat=%d message=%d error=%v",
					chatID,
					messageID,
					err,
				)
			}

		}(
			chatID,
			messageID,
			sourceText,
			targetLanguage,
		)

		// Don't send this group message to the normal AI handler.
		continue
	}
}
		// ---------------------------------------------------------
		// NORMAL AI CHAT
		// ---------------------------------------------------------

		go func(userStats *user.UsageTracker) {

			if userStats.HaveAccess(conf) {

				responseID :=
					api.HandleChatGPTStreamResponse(
						bot,
						client,
						update.Message,
						conf,
						userStats,
					)

				if conf.Model.Type == "openrouter" {
					userStats.GetUsageFromApi(
						responseID,
						conf,
					)
				}

			} else {

				msg := tgbotapi.NewMessage(
					update.Message.Chat.ID,
					lang.Translate(
						"budget_out",
						conf.Lang,
					),
				)

				if _, err := bot.Send(msg); err != nil {
					log.Println(err)
				}
			}

		}(userStats)
	}
}
