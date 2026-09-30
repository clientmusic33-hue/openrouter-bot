package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"openrouter-bot/api"
	"openrouter-bot/config"
	"openrouter-bot/lang"
	"openrouter-bot/translator"
	"openrouter-bot/ui"
	"openrouter-bot/user"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// quickActions maps the labels of the persistent reply keyboard to actions.
// Users never have to type a command: the keyboard is always there and its
// labels are handled like slash commands.
var quickActions = map[string]string{
	"🧠 model":       "models",
	"⚙️ settings":   "settings",
	"📊 usage":       "stats",
	"❓ help":        "help",
	"🤖 menu":        "menu",
	"📊 my usage":    "stats",
	"🔮 best for me": "recommend",
}

func quickAction(text string) (string, bool) {
	action, ok := quickActions[strings.ToLower(strings.TrimSpace(text))]

	return action, ok
}

// handleQuickAction serves a reply-keyboard button.
func (a *app) handleQuickAction(message *tgbotapi.Message, action string, conf *config.Config, tracker *user.UsageTracker) {
	switch action {
	case "models":
		a.sendScreen(message.Chat.ID, a.providersScreen(conf, tracker, 0))
	case "settings":
		a.sendScreen(message.Chat.ID, a.settingsScreen(conf, tracker))
	case "stats":
		a.sendScreen(message.Chat.ID, a.statsScreen(conf, tracker))
	case "recommend":
		a.sendScreen(message.Chat.ID, a.recommendScreen(conf, tracker))
	default:
		a.handleHelp(message, conf)
	}
}

// -----------------------------------------------------------------------------
// COMMANDS
// -----------------------------------------------------------------------------

func (a *app) handleCommand(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	chatID := message.Chat.ID
	command := message.Command()

	// Group access control also applies to commands, with the admin panels
	// handled separately below.
	if isGroupChat(message.Chat) && !a.mayUse(message.Chat, senderID(message), conf) {
		switch command {
		case "group", "admin":
			// handled below: admins are the only ones who may use them
		default:
			a.notifyRestricted(message, conf)
			return
		}
	}

	switch command {

	case "start":
		a.sendScreen(chatID, a.homeScreen(conf, tracker))

	case "menu":
		a.sendScreen(chatID, a.homeScreen(conf, tracker))

	case "help":
		a.handleHelp(message, conf)

	case "get_models":
		a.handleFreeModels(message, conf)

	case "set_model", "model":
		a.handleSetModel(message, conf, tracker)

	case "providers":
		a.sendScreen(chatID, a.providersScreen(conf, tracker, 0))

	case "provider":
		a.handleProvider(message, conf, tracker)

	case "models":
		a.handleModels(message, conf, tracker)

	case "auto":
		tracker.ResetPreference()
		a.send(chatID, "✨ Automatic mode on — I'll pick the best model for your usage.", "")
		a.sendScreen(chatID, a.homeScreen(conf, tracker))

	case "recommend", "rec":
		a.sendScreen(chatID, a.recommendScreen(conf, tracker))

	case "settings", "prefs":
		a.sendScreen(chatID, a.settingsScreen(conf, tracker))

	case "stats", "usage":
		a.sendScreen(chatID, a.statsScreen(conf, tracker))

	case "reset":
		a.handleReset(message, conf, tracker)

	case "stop":
		if tracker.StopStream() {
			a.send(chatID, lang.Translate("commands.stop", conf.Lang), "HTML")
			return
		}
		a.send(chatID, lang.Translate("commands.stop_err", conf.Lang), "HTML")

	case "about":
		a.send(chatID,
			"🤖 <b>OpenRouter AI Bot</b>\n\n"+
				"👨‍💻 <b>Created by:</b> @Hazel21_nut\n"+
				"⚡ <b>Providers:</b> "+escapeHTML(strings.Join(a.chain.UsableNames(), ", "))+"\n"+
				"🆓 <b>Free to use</b> · automatic failover · per-user model choice",
			"HTML",
		)

	case "tr":
		a.handleTranslateReply(message, conf)

	case "translate":
		a.handleTranslateSettings(message, conf)

	case "group":
		a.handleGroup(message, conf)

	case "admin":
		a.handleAdmin(message, conf)

	default:
		log.Printf("Unknown command: %s", message.Command())
	}
}

func (a *app) handleHelp(message *tgbotapi.Message, conf *config.Config) {
	text := "❓ <b>How to use me</b>\n\n" +
		"Just send a message — I answer with the best available model. ✨\n\n" +
		"<b>Buttons</b>\n" +
		"🧠 Model — pick a provider, then a model, or stay on auto\n" +
		"⚙️ Settings — footer, live typing, your own preferences\n" +
		"📊 Usage — your statistics\n" +
		"🔮 Best for me — a model suggestion based on your usage\n\n" +
		"<b>Commands</b>\n" +
		"/menu — the main panel\n" +
		"/model &lt;name&gt; — pin a model by id\n" +
		"/provider &lt;name&gt; — pin a provider\n" +
		"/auto — let me choose again\n" +
		"/reset — clear the conversation\n" +
		"/stop — stop the answer that is streaming\n" +
		"/group — group panel (admins)\n\n" +
		"Everything is free, no limits. 🤝"

	if isGroupChat(message.Chat) {
		text += "\n\nIn groups I only answer when you mention me or reply to me."
	}

	a.send(message.Chat.ID, text, "HTML")
}

// handleFreeModels lists the free models of the preferred provider.
func (a *app) handleFreeModels(message *tgbotapi.Message, conf *config.Config) {
	models, err := api.GetFreeModels(a.chain.ActiveBaseURL(), a.chain.ActiveAPIKey())
	if err != nil {
		log.Printf("Error getting models: %v", err)
		a.send(message.Chat.ID, "❌ Failed to get available models. Please try again later.", "")
		return
	}

	a.send(message.Chat.ID,
		lang.Translate("commands.getModels", conf.Lang)+models,
		tgbotapi.ModeMarkdown,
	)
}

// handleSetModel pins a model, by id or by catalogue number.
func (a *app) handleSetModel(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	args := strings.TrimSpace(message.CommandArguments())

	switch {
	case args == "", strings.EqualFold(args, "list"):
		a.sendScreen(message.Chat.ID, a.providersScreen(conf, tracker, 0))

	case strings.EqualFold(args, "default"), strings.EqualFold(args, "auto"):
		tracker.ResetPreference()
		a.send(message.Chat.ID, "✨ Automatic mode on — I'll pick the best model for your usage.", "")
		a.sendScreen(message.Chat.ID, a.homeScreen(conf, tracker))

	default:
		a.applyModelChoice(message, tracker, args)
	}
}

// applyModelChoice pins a model by id or by its number in the catalogue.
func (a *app) applyModelChoice(message *tgbotapi.Message, tracker *user.UsageTracker, choice string) {
	catalogue := a.chain.ModelCatalog()

	// A plain number selects from the catalogue, which is what /models shows.
	if index, err := strconv.Atoi(choice); err == nil {
		if index < 1 || index > len(catalogue) {
			a.send(message.Chat.ID, fmt.Sprintf("❌ Pick a number between 1 and %d.", len(catalogue)), "")
			return
		}
		ref := catalogue[index-1]
		tracker.SetModel(ref.Model, ref.Provider)
		a.send(message.Chat.ID, fmt.Sprintf("📌 Pinned <code>%s</code> on <b>%s</b>", escapeHTML(ref.Model), escapeHTML(ref.Provider)), "HTML")

		return
	}

	if strings.ContainsAny(choice, " \t") {
		a.send(message.Chat.ID, "❌ A model id has no spaces. Try /models to browse instead.", "")
		return
	}

	providerName, _ := a.chain.ProviderOf(choice)
	tracker.SetModel(choice, providerName)

	note := ""
	if providerName == "" {
		note = "\n⚠️ Not in the configured list — I'll try it first and fall back if it is unavailable."
	}

	a.send(message.Chat.ID, fmt.Sprintf("📌 Pinned <code>%s</code>%s", escapeHTML(choice), note), "HTML")
}

// handleProvider pins a provider (models stay on auto inside it).
func (a *app) handleProvider(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	args := strings.TrimSpace(message.CommandArguments())
	if args == "" {
		a.sendScreen(message.Chat.ID, a.providersScreen(conf, tracker, 0))
		return
	}

	if strings.EqualFold(args, "auto") || strings.EqualFold(args, "default") {
		tracker.ResetPreference()
		a.send(message.Chat.ID, "✨ Automatic mode on.", "")
		return
	}

	names := a.chain.Names()
	target := args
	if index, err := strconv.Atoi(args); err == nil && index >= 1 && index <= len(names) {
		target = names[index-1]
	}

	found := ""
	for _, name := range names {
		if strings.EqualFold(name, target) {
			found = name
			break
		}
	}

	if found == "" {
		a.send(message.Chat.ID, fmt.Sprintf(
			"❌ Unknown provider %s\n\nAvailable: %s",
			escapeHTML(target), escapeHTML(strings.Join(names, ", ")),
		), "HTML")
		return
	}

	tracker.SetProvider(found)
	a.send(message.Chat.ID, fmt.Sprintf("📌 Provider pinned to <b>%s</b>. Models inside it stay on auto. ✨", escapeHTML(found)), "HTML")
}

// handleModels lists one provider's models as a numbered list.
func (a *app) handleModels(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	args := strings.TrimSpace(message.CommandArguments())

	names := a.chain.Names()
	if args == "" {
		// Default to the provider the user is on, or the first one.
		if index := indexOfString(names, tracker.Preference().Provider); index >= 0 {
			a.sendScreen(message.Chat.ID, a.modelsScreen(conf, tracker, index, 0))
			return
		}
		a.sendScreen(message.Chat.ID, a.providersScreen(conf, tracker, 0))
		return
	}

	target := args
	if index, err := strconv.Atoi(args); err == nil && index >= 1 && index <= len(names) {
		target = names[index-1]
	}

	index := indexOfString(names, target)
	if index < 0 {
		a.send(message.Chat.ID, fmt.Sprintf(
			"❌ Unknown provider %s\n\nAvailable: %s",
			escapeHTML(args), escapeHTML(strings.Join(names, ", ")),
		), "HTML")
		return
	}

	a.sendScreen(message.Chat.ID, a.modelsScreen(conf, tracker, index, 0))
}

func indexOfString(values []string, target string) int {
	for i, value := range values {
		if strings.EqualFold(value, target) {
			return i
		}
	}

	return -1
}

func (a *app) handleReset(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	args := strings.TrimSpace(message.CommandArguments())

	tracker.ClearHistory()

	switch {
	case args == "":
		a.send(message.Chat.ID, lang.Translate("commands.reset", conf.Lang)+"\n🧠 Memory cleared — ask me anything.", "HTML")

	case strings.EqualFold(args, "system"):
		tracker.SetSystemPrompt(conf.SystemPrompt)
		a.send(message.Chat.ID, lang.Translate("commands.reset_system", conf.Lang), "HTML")

	default:
		tracker.SetSystemPrompt(args)
		a.send(message.Chat.ID,
			lang.Translate("commands.reset_prompt", conf.Lang)+escapeHTML(args)+".",
			"HTML",
		)
	}
}

// handleGroup opens the group panel for administrators.
func (a *app) handleGroup(message *tgbotapi.Message, conf *config.Config) {
	if !isGroupChat(message.Chat) {
		a.send(message.Chat.ID, "❌ /group only works inside a group.", "")
		return
	}

	if !a.isChatAdmin(message.Chat, senderID(message), conf) {
		a.send(message.Chat.ID, "🛡 Only group admins can open the group panel.", "")
		return
	}

	a.sendScreen(message.Chat.ID, a.groupScreen(conf, message.Chat))
}

// handleAdmin opens the owner panel.
func (a *app) handleAdmin(message *tgbotapi.Message, conf *config.Config) {
	if !conf.IsAdmin(senderID(message)) {
		a.send(message.Chat.ID, "👑 This panel is for the bot owner.", "")
		return
	}

	a.sendScreen(message.Chat.ID, a.adminScreen(conf))
}

// handleTranslateReply translates the message a user replied to.
func (a *app) handleTranslateReply(message *tgbotapi.Message, conf *config.Config) {
	if message.ReplyToMessage == nil {
		a.send(message.Chat.ID, "🌐 Reply to a message and use:\n\n/tr hi\n/tr en\n/tr ru", "")
		return
	}

	targetLanguage := strings.TrimSpace(message.CommandArguments())
	if targetLanguage == "" {
		targetLanguage = "English"
	}

	sourceText := strings.TrimSpace(messageText(message.ReplyToMessage))
	if sourceText == "" {
		a.send(message.Chat.ID, "❌ The replied message doesn't contain text.", "")
		return
	}

	ctx, cancel := context.WithTimeout(a.ctx, translationTimeout)
	defer cancel()

	translated, err := translator.Translate(ctx, a.chain, sourceText, targetLanguage)
	if err != nil {
		log.Printf("Translation error: %v", err)
		a.send(message.Chat.ID, "❌ Translation failed. Please try again.", "")
		return
	}

	a.send(message.Chat.ID, "🌐 <b>Translation</b>\n\n"+escapeHTML(translated), "HTML")
}

// handleTranslateSettings manages automatic group translation.
func (a *app) handleTranslateSettings(message *tgbotapi.Message, conf *config.Config) {
	if !isGroupChat(message.Chat) {
		a.send(message.Chat.ID, "❌ This command can only be used in a group.", "")
		return
	}

	chatID := message.Chat.ID
	args := strings.TrimSpace(message.CommandArguments())

	switch strings.ToLower(args) {

	case "on":
		if err := a.groups.SetEnabled(chatID, true); err != nil {
			log.Printf("Failed to enable translation: %v", err)
			a.send(chatID, "❌ Failed to save translation settings.", "")
			return
		}
		settings := a.groups.Get(chatID)
		a.send(chatID, fmt.Sprintf(
			"🌐 <b>Auto translation enabled</b>\nTarget language: <b>%s</b>",
			escapeHTML(settings.TargetLanguage),
		), "HTML")

	case "off":
		if err := a.groups.SetEnabled(chatID, false); err != nil {
			log.Printf("Failed to disable translation: %v", err)
			a.send(chatID, "❌ Failed to save translation settings.", "")
			return
		}
		a.send(chatID, "🌐 Auto translation disabled.", "")

	case "status", "":
		settings := a.groups.Get(chatID)
		status := "🔴 disabled"
		if settings.TranslateEnabled {
			status = "🟢 enabled"
		}
		a.send(chatID, fmt.Sprintf(
			"🌐 <b>Group translation</b>\n\n"+
				"Status: %s\nLanguage: <b>%s</b>\n\n"+
				"/translate on · /translate off\n"+
				"/translate hi — set Hindi and enable\n"+
				"/translate status",
			status, escapeHTML(settings.TargetLanguage),
		), "HTML")

	default:
		if err := a.groups.SetLanguage(chatID, args); err != nil {
			log.Printf("Failed to set translation language: %v", err)
			a.send(chatID, "❌ Failed to save translation language.", "")
			return
		}
		if err := a.groups.SetEnabled(chatID, true); err != nil {
			log.Printf("Failed to enable translation: %v", err)
			a.send(chatID, "❌ Language saved, but translation could not be enabled.", "")
			return
		}
		a.send(chatID, fmt.Sprintf("🌐 <b>Auto translation enabled</b>\nTarget language: <b>%s</b>", escapeHTML(args)), "HTML")
	}
}

// setCommands registers the command lists. Private and group chats get
// different menus: a group only needs the handful of commands that make sense
// in public.
func (a *app) setCommands(conf *config.Config) {
	private := []tgbotapi.BotCommand{
		{Command: "menu", Description: "Main panel with buttons"},
		{Command: "model", Description: "Pick a provider and a model"},
		{Command: "auto", Description: "Let the bot choose the model"},
		{Command: "settings", Description: "Your preferences"},
		{Command: "recommend", Description: "Best model for your usage"},
		{Command: "stats", Description: "Your usage statistics"},
		{Command: "reset", Description: "Start a new conversation"},
		{Command: "stop", Description: "Stop the running answer"},
		{Command: "help", Description: "How to use the bot"},
		{Command: "about", Description: "About this bot"},
	}

	group := []tgbotapi.BotCommand{
		{Command: "menu", Description: "Main panel with buttons"},
		{Command: "model", Description: "Pick a provider and a model"},
		{Command: "auto", Description: "Let the bot choose the model"},
		{Command: "recommend", Description: "Best model for your usage"},
		{Command: "translate", Description: "Auto-translate this group"},
		{Command: "tr", Description: "Translate the replied message"},
		{Command: "group", Description: "Group admin panel"},
		{Command: "help", Description: "How to use the bot"},
	}

	if _, err := a.bot.Request(tgbotapi.NewSetMyCommandsWithScope(
		tgbotapi.NewBotCommandScopeDefault(), private...,
	)); err != nil {
		log.Printf("Warning: failed to set default commands: %v", err)
	}

	if _, err := a.bot.Request(tgbotapi.NewSetMyCommandsWithScope(
		tgbotapi.NewBotCommandScopeAllGroupChats(), group...,
	)); err != nil {
		log.Printf("Warning: failed to set group commands: %v", err)
	}

	if _, err := a.bot.Request(tgbotapi.NewSetMyCommandsWithScope(
		tgbotapi.NewBotCommandScopeAllPrivateChats(), private...,
	)); err != nil {
		log.Printf("Warning: failed to set private chat commands: %v", err)
	}

	if _, err := a.bot.Request(tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "start", Description: lang.Translate("description.start", conf.Lang)},
	)); err != nil {
		log.Printf("Warning: failed to set start command: %v", err)
	}
}

// useModel pins the model carried by a callback, looking its provider up.
func (a *app) useModel(chatID int64, messageID int, tracker *user.UsageTracker, model string) {
	if model == "" {
		return
	}

	providerName, known := a.chain.ProviderOf(model)
	tracker.SetModel(model, providerName)

	note := ""
	if !known {
		note = "\n⚠️ Not in the configured list — it will be tried first."
	}

	a.editScreen(chatID, messageID, screen{
		Text: fmt.Sprintf("📌 Pinned <code>%s</code>%s", escapeHTML(model), note),
		Keyboard: tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🧠 Change", ui.Encode(ui.ActionProviders)),
				tgbotapi.NewInlineKeyboardButtonData("🏠 Menu", ui.Encode(ui.ActionMenu)),
			),
		),
	})
}
