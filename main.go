// Command openrouter-bot runs a Telegram bot that answers with AI models
// served through OpenRouter or any OpenAI-compatible endpoint.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"openrouter-bot/api"
	"openrouter-bot/config"
	"openrouter-bot/grouptranslate"
	"openrouter-bot/lang"
	"openrouter-bot/provider"
	"openrouter-bot/translator"
	"openrouter-bot/user"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	shutdownTimeout   = 30 * time.Second
	translationTimout = 60 * time.Second
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---------------------------------------------------------------------
	// TRANSLATIONS
	// ---------------------------------------------------------------------
	//
	// Loaded exactly once. Reloading per message rewrote a package level map
	// that other goroutines were reading concurrently.

	if err := lang.LoadTranslations("./lang/"); err != nil {
		log.Fatalf("Error loading translations: %v", err)
	}

	// ---------------------------------------------------------------------
	// CONFIGURATION
	// ---------------------------------------------------------------------

	manager, err := config.NewManager("./config.yaml")
	if err != nil {
		log.Fatalf("Error initializing config manager: %v", err)
	}

	conf := manager.GetConfig()

	// ---------------------------------------------------------------------
	// TELEGRAM BOT
	// ---------------------------------------------------------------------

	if conf.TelegramBotToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}

	bot, err := tgbotapi.NewBotAPI(conf.TelegramBotToken)
	if err != nil {
		log.Fatalf("Failed to create Telegram bot: %v", err)
	}

	bot.Debug = false
	log.Printf("Authorized as @%s", bot.Self.UserName)

	if _, err := bot.Request(tgbotapi.DeleteWebhookConfig{}); err != nil {
		log.Printf("Warning: failed to delete webhook: %v", err)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	// ---------------------------------------------------------------------
	// PROVIDER CHAIN
	// ---------------------------------------------------------------------

	providers := conf.Providers
	if len(providers) == 0 {
		// No providers: chain in config.yaml, so fall back to the flat
		// environment variables.
		providers = config.FallbackProviders(conf)
	}

	chain, err := provider.NewChain(providers)
	if err != nil {
		log.Fatalf("Failed to build provider chain: %v", err)
	}

	for _, status := range chain.Status() {
		if status.Configured {
			log.Printf("Provider %s ready: %d model(s)", status.Name, len(status.Models))
		} else {
			log.Printf("Provider %s configured without an API key, requests to it will fail", status.Name)
		}
	}

	// ---------------------------------------------------------------------
	// APPLICATION
	// ---------------------------------------------------------------------

	app := &app{
		ctx:          ctx,
		bot:          bot,
		manager:      manager,
		chain:        chain,
		users:        user.NewUserManager("logs"),
		translations: grouptranslate.NewManager("data/translations.json"),
		semaphore:    make(chan struct{}, conf.MaxConcurrentRequests),
	}

	app.setCommands(manager.GetConfig())

	// ---------------------------------------------------------------------
	// HEALTH SERVER
	// ---------------------------------------------------------------------

	server := app.startHealthServer()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	// ---------------------------------------------------------------------
	// UPDATE LOOP
	// ---------------------------------------------------------------------

	app.ready.Store(true)
	log.Println("Bot is running, waiting for updates")

	app.run(updates)

	// ---------------------------------------------------------------------
	// GRACEFUL SHUTDOWN
	// ---------------------------------------------------------------------

	log.Println("Shutting down, waiting for in-flight requests")

	bot.StopReceivingUpdates()

	done := make(chan struct{})
	go func() {
		app.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("All requests finished")
	case <-time.After(shutdownTimeout):
		log.Printf("Timed out after %s waiting for requests to finish", shutdownTimeout)
	}

	log.Println("Goodbye")
}

// -----------------------------------------------------------------------------
// APPLICATION
// -----------------------------------------------------------------------------

type app struct {
	ctx     context.Context
	bot     *tgbotapi.BotAPI
	manager *config.Manager

	// chain is the ordered list of AI providers tried on every request.
	chain *provider.Chain

	users        *user.Manager
	translations *grouptranslate.Manager

	// semaphore caps concurrent AI requests so that a burst of group
	// messages cannot exhaust the upstream rate limit.
	semaphore chan struct{}

	wg    sync.WaitGroup
	ready atomic.Bool
}

// run consumes Telegram updates until the channel closes or the context is
// cancelled.
func (a *app) run(updates tgbotapi.UpdatesChannel) {
	for {
		select {
		case <-a.ctx.Done():
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			a.processUpdate(update)
		}
	}
}

func (a *app) processUpdate(update tgbotapi.Update) {
	if update.Message == nil {
		return
	}

	// Ignore messages sent by other bots.
	if update.Message.From != nil && update.Message.From.IsBot {
		return
	}

	senderID, senderUsername := senderInfo(update.Message)
	if senderID == 0 {
		log.Printf("Ignoring message without a resolvable sender in chat %d", update.Message.Chat.ID)
		return
	}

	// Read a fresh snapshot: the configuration may have been reloaded.
	conf := a.manager.GetConfig()

	tracker := a.users.GetUser(update.Message.Chat.ID, senderID, senderUsername, conf)

	log.Printf(
		"INCOMING MESSAGE: chat=%d type=%s sender_id=%d username=%q text=%q",
		update.Message.Chat.ID,
		update.Message.Chat.Type,
		senderID,
		senderUsername,
		update.Message.Text,
	)

	if update.Message.IsCommand() {
		a.handleCommand(update.Message, conf, tracker)
		return
	}

	// -----------------------------------------------------------------
	// AUTOMATIC GROUP TRANSLATION
	// -----------------------------------------------------------------

	if isGroupChat(update.Message.Chat) {
		settings := a.translations.Get(update.Message.Chat.ID)
		text := strings.TrimSpace(messageText(update.Message))

		if settings.Enabled && text != "" {
			a.startTranslation(update.Message, settings.TargetLanguage, conf)

			// Translation mode replaces the chat mode for this group.
			// Without this return every message would also trigger a full
			// AI completion.
			return
		}
	}

	// -----------------------------------------------------------------
	// NORMAL AI CHAT
	// -----------------------------------------------------------------

	if !a.shouldAnswer(update.Message, conf) {
		return
	}

	a.wg.Add(1)
	go a.handleChat(update.Message, conf, tracker)
}

// handleChat runs one AI request. It is started in its own goroutine and
// bounded by the concurrency semaphore.
func (a *app) handleChat(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	defer a.wg.Done()

	select {
	case a.semaphore <- struct{}{}:
		defer func() { <-a.semaphore }()
	case <-a.ctx.Done():
		return
	}

	if !tracker.HaveAccess(conf) {
		sendMessage(a.bot, message.Chat.ID, lang.Translate("budget_out", conf.Lang), "")
		return
	}

	if err := tracker.AllowRequest(conf); err != nil {
		log.Printf("Rate limiting user %s: %v", tracker.UserID, err)
		sendMessage(a.bot, message.Chat.ID, lang.Translate("rate_limit", conf.Lang), "")
		return
	}

	log.Printf(
		"AI REQUEST: chat=%d sender_id=%s username=%q text=%q",
		message.Chat.ID,
		tracker.UserID,
		tracker.UserName,
		message.Text,
	)

	tracker.RecordRequest(len(messageText(message)))
	if len(message.Photo) > 0 {
		tracker.MarkVision()
	}

	responseID, servedBy, err := api.HandleChatGPTStreamResponse(
		a.ctx,
		a.bot,
		a.chain,
		message,
		conf,
		tracker,
	)
	if err != nil {
		log.Printf("AI request failed for user %s: %v", tracker.UserID, err)
	}

	// Cost statistics are an OpenRouter specific endpoint.
	if servedBy == "openrouter" && conf.Model.Type == "openrouter" {
		if err := tracker.GetUsageFromApi(responseID, conf); err != nil {
			log.Printf("Failed to fetch usage for user %s: %v", tracker.UserID, err)
		}
	}
}

// -----------------------------------------------------------------------------
// GROUP BEHAVIOUR
// -----------------------------------------------------------------------------

// shouldAnswer decides whether the bot answers a non-command message.
func (a *app) shouldAnswer(message *tgbotapi.Message, conf *config.Config) bool {
	if !isGroupChat(message.Chat) {
		return true
	}

	switch conf.GroupChatMode {
	case config.GroupModeOff:
		return false
	case config.GroupModeAll:
		return true
	default: // config.GroupModeMention
		if message.ReplyToMessage != nil &&
			message.ReplyToMessage.From != nil &&
			message.ReplyToMessage.From.ID == a.bot.Self.ID {
			return true
		}
		return a.isMentioned(message)
	}
}

func (a *app) isMentioned(message *tgbotapi.Message) bool {
	username := strings.ToLower(a.bot.Self.UserName)
	if username == "" {
		return false
	}

	return strings.Contains(strings.ToLower(messageText(message)), "@"+username)
}

func (a *app) startTranslation(message *tgbotapi.Message, target string, conf *config.Config) {
	chatID := message.Chat.ID
	messageID := message.MessageID
	text := strings.TrimSpace(messageText(message))

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		ctx, cancel := context.WithTimeout(a.ctx, translationTimout)
		defer cancel()

		log.Printf("Starting automatic translation: chat=%d message=%d target=%q", chatID, messageID, target)

		translated, err := translator.Translate(ctx, a.chain.ActiveClient(), text, target, a.chain.Model())
		if err != nil {
			log.Printf("Automatic translation FAILED: chat=%d message=%d error=%v", chatID, messageID, err)
			return
		}

		log.Printf("Automatic translation SUCCESS: chat=%d message=%d", chatID, messageID)

		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🌐 <b>%s</b>\n\n%s", escapeHTML(target), escapeHTML(translated)))
		msg.ParseMode = "HTML"
		msg.ReplyToMessageID = messageID

		if _, err := a.bot.Send(msg); err != nil {
			log.Printf("Failed to send automatic translation: chat=%d message=%d error=%v", chatID, messageID, err)
		}
	}()
}

// -----------------------------------------------------------------------------
// COMMANDS
// -----------------------------------------------------------------------------

func (a *app) handleCommand(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	chatID := message.Chat.ID

	switch message.Command() {

	case "start":
		sendMessage(a.bot, chatID,
			lang.Translate("commands.start", conf.Lang)+
				lang.Translate("commands.help", conf.Lang)+
				lang.Translate("commands.start_end", conf.Lang),
			"HTML",
		)

	case "help":
		sendMessage(a.bot, chatID, lang.Translate("commands.help", conf.Lang), "HTML")

	case "get_models":
		models, err := api.GetFreeModels(a.chain.ActiveBaseURL(), a.chain.ActiveAPIKey())
		if err != nil {
			log.Printf("Error getting models: %v", err)
			sendMessage(a.bot, chatID, "❌ Failed to get available models. Please try again later.", "")
			return
		}

		sendMessage(a.bot, chatID,
			lang.Translate("commands.getModels", conf.Lang)+models,
			tgbotapi.ModeMarkdown,
		)

	case "set_model", "model":
		a.handleSetModel(message, conf)

	case "providers":
		a.handleProviders(message, conf)

	case "provider":
		a.handleProvider(message, conf)

	case "models":
		a.handleModels(message, conf)

	case "recommend":
		a.handleRecommend(message, conf, tracker)

	case "reset":
		a.handleReset(message, conf, tracker)

	case "stats":
		a.handleStats(message, conf, tracker)

	case "stop":
		if tracker.StopStream() {
			sendMessage(a.bot, chatID, lang.Translate("commands.stop", conf.Lang), "HTML")
			return
		}
		sendMessage(a.bot, chatID, lang.Translate("commands.stop_err", conf.Lang), "HTML")

	case "about":
		sendMessage(a.bot, chatID,
			"🤖 <b>OpenRouter AI Bot</b>\n\n"+
				"👨‍💻 <b>Created by:</b> @Hazel21_nut\n"+
				"⚡ <b>Powered by:</b> OpenRouter",
			"HTML",
		)

	case "tr":
		a.handleTranslateReply(message, conf)

	case "translate":
		a.handleTranslateSettings(message, conf)

	default:
		log.Printf("Unknown command: %s", message.Command())
	}
}

func (a *app) handleSetModel(message *tgbotapi.Message, conf *config.Config) {
	args := strings.TrimSpace(message.CommandArguments())

	switch {
	case strings.EqualFold(args, "default"):
		model := a.chain.ResetModel()
		sendMessage(a.bot, message.Chat.ID,
			lang.Translate("commands.setModel", conf.Lang)+" `"+model+"`",
			tgbotapi.ModeMarkdown,
		)

	case args == "":
		sendMessage(a.bot, message.Chat.ID,
			lang.Translate("commands.noArgsModel", conf.Lang),
			tgbotapi.ModeMarkdown,
		)

	case strings.ContainsAny(args, " \t"):
		sendMessage(a.bot, message.Chat.ID,
			lang.Translate("commands.noSpaceModel", conf.Lang),
			tgbotapi.ModeMarkdown,
		)

	default:
		if err := a.chain.SetModel(args); err != nil {
			sendMessage(a.bot, message.Chat.ID, "❌ "+escapeHTML(err.Error()), "")
			return
		}
		log.Printf("Model changed to %s in chat %d", args, message.Chat.ID)
		sendMessage(a.bot, message.Chat.ID,
			lang.Translate("commands.setModel", conf.Lang)+" `"+args+"`",
			tgbotapi.ModeMarkdown,
		)
	}
}

func (a *app) handleReset(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	args := strings.TrimSpace(message.CommandArguments())

	tracker.ClearHistory()

	switch {
	case args == "":
		sendMessage(a.bot, message.Chat.ID, lang.Translate("commands.reset", conf.Lang), "HTML")

	case strings.EqualFold(args, "system"):
		tracker.SetSystemPrompt(conf.SystemPrompt)
		sendMessage(a.bot, message.Chat.ID, lang.Translate("commands.reset_system", conf.Lang), "HTML")

	default:
		tracker.SetSystemPrompt(args)
		sendMessage(a.bot, message.Chat.ID,
			lang.Translate("commands.reset_prompt", conf.Lang)+escapeHTML(args)+".",
			"HTML",
		)
	}
}

func (a *app) handleStats(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	tracker.CheckHistory(conf.MaxHistorySize, conf.MaxHistoryTime)

	countedUsage := formatMoney(tracker.GetCurrentCost(conf.BudgetPeriod))
	todayUsage := formatMoney(tracker.GetCurrentCost("daily"))
	monthUsage := formatMoney(tracker.GetCurrentCost("monthly"))
	totalUsage := formatMoney(tracker.GetCurrentCost("total"))
	messagesCount := strconv.Itoa(len(tracker.GetMessages()))

	var statsMessage string
	if tracker.CanViewStats(conf) {
		statsMessage = fmt.Sprintf(
			lang.Translate("commands.stats", conf.Lang),
			countedUsage, todayUsage, monthUsage, totalUsage, messagesCount,
		)
	} else {
		statsMessage = fmt.Sprintf(
			lang.Translate("commands.stats_min", conf.Lang),
			messagesCount,
		)
	}

	sendMessage(a.bot, message.Chat.ID, statsMessage, "HTML")
}

func (a *app) handleTranslateReply(message *tgbotapi.Message, conf *config.Config) {
	if message.ReplyToMessage == nil {
		sendMessage(a.bot, message.Chat.ID,
			"❌ Reply to a message and use:\n\n/tr hi\n/tr en\n/tr ru", "")
		return
	}

	targetLanguage := strings.TrimSpace(message.CommandArguments())
	if targetLanguage == "" {
		targetLanguage = "English"
	}

	sourceText := strings.TrimSpace(messageText(message.ReplyToMessage))
	if sourceText == "" {
		sendMessage(a.bot, message.Chat.ID, "❌ The replied message doesn't contain text.", "")
		return
	}

	ctx, cancel := context.WithTimeout(a.ctx, translationTimout)
	defer cancel()

	translated, err := translator.Translate(ctx, a.chain.ActiveClient(), sourceText, targetLanguage, a.chain.Model())
	if err != nil {
		log.Printf("Translation error: %v", err)
		sendMessage(a.bot, message.Chat.ID, "❌ Translation failed. Please try again.", "")
		return
	}

	sendMessage(a.bot, message.Chat.ID,
		"🌐 <b>Translation</b>\n\n"+escapeHTML(translated),
		"HTML",
	)
}

func (a *app) handleTranslateSettings(message *tgbotapi.Message, conf *config.Config) {
	if !isGroupChat(message.Chat) {
		sendMessage(a.bot, message.Chat.ID, "❌ This command can only be used in a group.", "")
		return
	}

	chatID := message.Chat.ID
	args := strings.TrimSpace(message.CommandArguments())

	switch strings.ToLower(args) {

	case "on":
		if err := a.translations.SetEnabled(chatID, true); err != nil {
			log.Printf("Failed to enable translation: %v", err)
			sendMessage(a.bot, chatID, "❌ Failed to save translation settings.", "")
			return
		}
		settings := a.translations.Get(chatID)
		sendMessage(a.bot, chatID, fmt.Sprintf(
			"🌐 <b>Auto Translation Enabled</b>\n\n"+
				"Target language: <b>%s</b>\n\n"+
				"New group messages will now be translated automatically.",
			escapeHTML(settings.TargetLanguage),
		), "HTML")

	case "off":
		if err := a.translations.SetEnabled(chatID, false); err != nil {
			log.Printf("Failed to disable translation: %v", err)
			sendMessage(a.bot, chatID, "❌ Failed to save translation settings.", "")
			return
		}
		sendMessage(a.bot, chatID, "🌐 Auto translation disabled.", "")

	case "status":
		settings := a.translations.Get(chatID)
		status := "🔴 Disabled"
		if settings.Enabled {
			status = "🟢 Enabled"
		}
		sendMessage(a.bot, chatID, fmt.Sprintf(
			"🌐 <b>Translation Status</b>\n\n"+
				"Status: %s\n"+
				"Language: <b>%s</b>\n"+
				"Mode: Automatic",
			status,
			escapeHTML(settings.TargetLanguage),
		), "HTML")

	case "":
		sendMessage(a.bot, chatID,
			"🌐 <b>Group Translation</b>\n\n"+
				"/translate on — Enable\n"+
				"/translate off — Disable\n"+
				"/translate hi — Set Hindi and enable\n"+
				"/translate en — Set English and enable\n"+
				"/translate status — Show settings",
			"HTML",
		)

	default:
		if err := a.translations.SetLanguage(chatID, args); err != nil {
			log.Printf("Failed to set translation language: %v", err)
			sendMessage(a.bot, chatID, "❌ Failed to save translation language.", "")
			return
		}
		if err := a.translations.SetEnabled(chatID, true); err != nil {
			log.Printf("Failed to enable translation: %v", err)
			sendMessage(a.bot, chatID, "❌ Language was saved, but translation could not be enabled.", "")
			return
		}
		settings := a.translations.Get(chatID)
		sendMessage(a.bot, chatID, fmt.Sprintf(
			"🌐 <b>Auto Translation Enabled</b>\n\n"+
				"Target language: <b>%s</b>\n\n"+
				"You don't need to specify the language again.",
			escapeHTML(settings.TargetLanguage),
		), "HTML")
	}
}

func (a *app) setCommands(conf *config.Config) {
	commands := []tgbotapi.BotCommand{
		{Command: "start", Description: lang.Translate("description.start", conf.Lang)},
		{Command: "help", Description: lang.Translate("description.help", conf.Lang)},
		{Command: "get_models", Description: lang.Translate("description.getModels", conf.Lang)},
		{Command: "set_model", Description: lang.Translate("description.setModel", conf.Lang)},
		{Command: "providers", Description: "List AI providers and their health"},
		{Command: "provider", Description: "Switch the active AI provider"},
		{Command: "models", Description: "List models of a provider"},
		{Command: "recommend", Description: "Recommend a model based on your usage"},
		{Command: "reset", Description: lang.Translate("description.reset", conf.Lang)},
		{Command: "stats", Description: lang.Translate("description.stats", conf.Lang)},
		{Command: "stop", Description: lang.Translate("description.stop", conf.Lang)},
		{Command: "about", Description: "About this bot"},
		{Command: "tr", Description: "Translate a replied message"},
		{Command: "translate", Description: "Manage group auto translation"},
	}

	if _, err := a.bot.Request(tgbotapi.NewSetMyCommands(commands...)); err != nil {
		log.Printf("Warning: failed to set bot commands: %v", err)
	}
}

// -----------------------------------------------------------------------------
// PROVIDER AND MODEL COMMANDS
// -----------------------------------------------------------------------------

// handleProviders shows the failover chain and its health.
func (a *app) handleProviders(message *tgbotapi.Message, conf *config.Config) {
	status := a.chain.Status()

	var builder strings.Builder

	builder.WriteString("🌐 <b>AI providers</b>\n\n")

	for i, info := range status {
		marker := "  "
		if info.Active {
			marker = "▶ "
		}

		state := "🟢"
		switch {
		case !info.Configured:
			state = "🔑" // missing API key
		case info.Cooldown > 0:
			state = "⏳"
		case info.Failures > 0:
			state = "🟡"
		}

		builder.WriteString(fmt.Sprintf(
			"<code>%d</code> %s %s <b>%s</b> · %d model(s)\n",
			i+1, marker, state, escapeHTML(info.Name), len(info.Models),
		))

		detail := fmt.Sprintf(
			"     current: <code>%s</code>",
			escapeHTML(a.chain.Model()),
		)
		if info.Cooldown > 0 {
			detail += fmt.Sprintf(" · cooling down %s", info.Cooldown)
		}
		if info.Failures > 0 {
			detail += fmt.Sprintf(" · %d failure(s)", info.Failures)
		}
		if !info.Configured {
			detail += " · no API key"
		}
		if info.Active {
			builder.WriteString(detail + "\n")
		}
	}

	builder.WriteString(fmt.Sprintf(
		"\nCurrent model: <code>%s</code>\n",
		escapeHTML(a.chain.Model()),
	))
	builder.WriteString("\nSwitch with <code>/provider 2</code>, list models with <code>/models</code>.")

	sendMessage(a.bot, message.Chat.ID, builder.String(), "HTML")
}

// handleProvider switches the preferred provider by number or name.
func (a *app) handleProvider(message *tgbotapi.Message, conf *config.Config) {
	args := strings.TrimSpace(message.CommandArguments())
	if args == "" {
		a.handleProviders(message, conf)
		return
	}

	names := a.chain.Names()

	target := args
	if index, err := strconv.Atoi(args); err == nil && index >= 1 && index <= len(names) {
		target = names[index-1]
	}

	name, err := a.chain.SetActive(target)
	if err != nil {
		sendMessage(a.bot, message.Chat.ID,
			fmt.Sprintf("❌ %s\n\nAvailable: %s", escapeHTML(err.Error()), escapeHTML(strings.Join(names, ", "))),
			"HTML",
		)
		return
	}

	sendMessage(a.bot, message.Chat.ID, fmt.Sprintf(
		"🌐 Provider switched to <b>%s</b>\n\nModel: <code>%s</code>",
		escapeHTML(name), escapeHTML(a.chain.Model()),
	), "HTML")
}

// handleModels lists the models of a provider with their capabilities.
func (a *app) handleModels(message *tgbotapi.Message, conf *config.Config) {
	args := strings.TrimSpace(message.CommandArguments())

	providerName := a.chain.ActiveName()
	if args != "" {
		names := a.chain.Names()
		if index, err := strconv.Atoi(args); err == nil && index >= 1 && index <= len(names) {
			providerName = names[index-1]
		} else {
			providerName = args
		}
	}

	models, ok := a.chain.ModelsOf(providerName)
	if !ok {
		sendMessage(a.bot, message.Chat.ID, fmt.Sprintf(
			"❌ Unknown provider %q.\n\nAvailable: %s",
			escapeHTML(args), escapeHTML(strings.Join(a.chain.Names(), ", ")),
		), "HTML")
		return
	}

	current := a.chain.Model()

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("🧠 <b>Models on %s</b>\n\n", escapeHTML(providerName)))

	for i, model := range models {
		marker := "  "
		if model == current {
			marker = "▶ "
		}

		line := fmt.Sprintf("<code>%d</code> %s <code>%s</code>", i+1, marker, escapeHTML(model))
		if description := provider.DescribeModel(model); description != "" {
			line += " — " + escapeHTML(description)
		}
		builder.WriteString(line + "\n")
	}

	builder.WriteString("\nSelect with <code>/model 2</code> or <code>/set_model &lt;name&gt;</code>.")

	sendMessage(a.bot, message.Chat.ID, builder.String(), "HTML")
}

// handleRecommend suggests a model based on how the user actually uses the bot.
func (a *app) handleRecommend(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	profile := tracker.UsageProfile()

	// Penalise backends that are currently failing.
	for _, info := range a.chain.Status() {
		profile.FailedRecently += info.Failures
	}

	recommendation, err := a.chain.Recommend(profile)
	if err != nil {
		sendMessage(a.bot, message.Chat.ID, "❌ No models available to recommend.", "")
		return
	}

	historyNote := "no history yet"
	if profile.HistoryMessages > 0 {
		historyNote = fmt.Sprintf("%d messages in memory", profile.HistoryMessages)
	}

	visionNote := "no"
	if profile.UsesVision {
		visionNote = "yes"
	}

	text := fmt.Sprintf(
		"🔮 <b>Recommended model</b>\n\n"+
			"<code>%s</code> on <b>%s</b>\n\n"+
			"Why: %s\n\n"+
			"Your usage: ~%.0f requests/day · avg prompt %.0f chars · %s · images: %s\n\n"+
			"Apply with <code>/model %s</code>",
		escapeHTML(recommendation.Model),
		escapeHTML(recommendation.Provider),
		escapeHTML(recommendation.Reason),
		profile.RequestsPerDay,
		profile.AvgPromptChars,
		historyNote,
		visionNote,
		escapeHTML(recommendation.Model),
	)

	sendMessage(a.bot, message.Chat.ID, text, "HTML")
}

// -----------------------------------------------------------------------------
// HEALTH SERVER
// -----------------------------------------------------------------------------

func (a *app) startHealthServer() *http.Server {
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OpenRouter Telegram Bot is running"))
	})

	// /healthz reports real readiness: it fails until the bot is polling and
	// returns 503 once the process is shutting down.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !a.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("starting"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	server := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("HTTP server listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Never Fatalf from a goroutine: it would kill the bot because a
			// port happened to be busy.
			a.ready.Store(false)
			log.Printf("HTTP server failed: %v", err)
		}
	}()

	return server
}

// -----------------------------------------------------------------------------
// HELPERS
// -----------------------------------------------------------------------------

// sendMessage sends text, splitting it into Telegram sized chunks and
// retrying without formatting if the parse mode is rejected.
func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string, parseMode string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	for _, chunk := range api.SplitMessage(text, config.ChunkLimit, 0) {
		if parseMode != "" {
			msg := tgbotapi.NewMessage(chatID, chunk)
			msg.ParseMode = parseMode

			_, err := bot.Send(msg)
			if err == nil {
				continue
			}

			// Telegram rejects the whole message when it cannot parse the
			// markup, so retry this chunk without formatting rather than
			// losing it.
			log.Printf("Sending chunk with parse mode %q failed, retrying as plain text: %v", parseMode, err)
		}

		msg := tgbotapi.NewMessage(chatID, chunk)
		if _, err := bot.Send(msg); err != nil {
			log.Printf("Failed to send message to chat %d: %v", chatID, err)
			return err
		}
	}

	return nil
}

func senderInfo(message *tgbotapi.Message) (int64, string) {
	if message.From != nil {
		return message.From.ID, message.From.UserName
	}
	if message.SenderChat != nil {
		return message.SenderChat.ID, message.SenderChat.UserName
	}

	return 0, ""
}

// messageText returns the text or, for media messages, the caption.
func messageText(message *tgbotapi.Message) string {
	if message == nil {
		return ""
	}
	if message.Text != "" {
		return message.Text
	}

	return message.Caption
}

func isGroupChat(chat *tgbotapi.Chat) bool {
	return chat != nil && (chat.Type == "group" || chat.Type == "supergroup")
}

func formatMoney(value float64) string {
	return strconv.FormatFloat(value, 'f', 6, 64)
}

// escapeHTML escapes user and model supplied text before it is embedded in an
// HTML formatted Telegram message.
func escapeHTML(text string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)

	return replacer.Replace(text)
}
