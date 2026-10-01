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

	// Access control applies to commands too, so that a restricted user
	// cannot open panels they are not allowed to use. The admin panels are
	// handled separately below, because the people who may use them are
	// exactly the ones this gate would turn away.
	if !a.mayUse(message.Chat, senderID(message), conf) {
		if !isGroupChat(message.Chat) {
			a.warnRestrictedPrivate(chatID, senderID(message), conf)
			return
		}

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

	case "reset", "new":
		a.handleReset(message, conf, tracker)

	case "fast":
		a.handleFast(message, conf, tracker)

	case "race":
		a.handleRace(message, conf, tracker)

	case "research", "web":
		a.handleResearchCommand(message, conf, tracker)

	case "agent":
		a.handleAgentCommand(message, conf, tracker)

	case "memory", "mem":
		a.handleMemoryCommand(message, conf, tracker)

	case "persona":
		a.handlePersonaCommand(message, conf, tracker)

	case "summarize", "summary":
		a.handleSummarizeCommand(message, conf, tracker)

	case "remind":
		a.handleRemindCommand(message, tracker)

	case "reminders":
		a.handleRemindersListCommand(message, tracker)

	case "note":
		a.handleNoteCommand(message, tracker)

	case "notes":
		a.handleNotesListCommand(message, tracker)

	case "task":
		a.handleTaskCommand(message, tracker)

	case "tasks", "todo":
		a.handleTasksListCommand(message, tracker)

	case "code", "review", "explain", "testgen":
		a.handleCodeCommand(message, conf, tracker)

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
		"Just send a message, photo, voice note, or document — I answer with the best available model. ✨\n\n" +
		"<b>Buttons</b>\n" +
		"🧠 Model — pick a provider, then a model, or stay on auto\n" +
		"🎭 Persona — Developer, Teacher, Researcher, Writer, Translator, Coding Agent, Business\n" +
		"🗂 Memory — view or manage persistent facts\n" +
		"⚙️ Settings — footer, live typing, preferences\n\n" +
		"<b>AI &amp; Speed</b>\n" +
		"/fast &lt;prompt&gt; — lowest-latency model\n" +
		"/race &lt;prompt&gt; — race 2–3 models concurrently\n" +
		"/research &lt;topic&gt; — web research with cited sources\n" +
		"/agent &lt;task&gt; — multi-step tool-using agent\n" +
		"/code · /review · /explain · /testgen — coding assistant\n\n" +
		"<b>Memory, Personas &amp; Productivity</b>\n" +
		"/persona [name] — switch AI persona\n" +
		"/memory [list|add|forget|clear|off] — long-term memory\n" +
		"/remind &lt;30m|14:30&gt; &lt;text&gt; · /reminders\n" +
		"/note &lt;text&gt; · /notes · /task &lt;text&gt; · /tasks\n" +
		"/summarize — summarize conversation &amp; action items\n" +
		"/tr &lt;lang&gt; — translate replied message or text\n" +
		"/model · /provider · /auto · /reset · /stop · /group\n\n" +
		"Everything is free, fast, and resilient. 🤝"

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

// handleTranslateReply translates the message a user replied to, or inline text
// passed as `/tr <lang> <text>`.
func (a *app) handleTranslateReply(message *tgbotapi.Message, conf *config.Config) {
	args := strings.TrimSpace(message.CommandArguments())

	targetLanguage := "English"
	sourceText := ""

	if message.ReplyToMessage != nil {
		if args != "" {
			targetLanguage = args
		}
		sourceText = strings.TrimSpace(messageText(message.ReplyToMessage))
	} else if args != "" {
		parts := strings.SplitN(args, " ", 2)
		if len(parts) == 2 {
			targetLanguage = strings.TrimSpace(parts[0])
			sourceText = strings.TrimSpace(parts[1])
		} else {
			sourceText = args
		}
	} else {
		a.send(message.Chat.ID, "🌐 Reply to a message and use:\n\n/tr hi\n/tr en\n/tr ru\n\nOr translate inline:\n/tr es Hello world", "")
		return
	}

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

	detected := translator.DetectLanguage(sourceText)
	header := "🌐 <b>Translation</b>"
	if detected != "" && detected != "Unknown" {
		header = fmt.Sprintf("🌐 <b>Translation</b> (%s → %s)", escapeHTML(detected), escapeHTML(targetLanguage))
	}
	a.send(message.Chat.ID, header+"\n\n"+escapeHTML(translated), "HTML")
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
		{Command: "fast", Description: "Fast low-latency AI response"},
		{Command: "race", Description: "Race multiple AI models concurrently"},
		{Command: "research", Description: "Web research with cited sources"},
		{Command: "agent", Description: "Multi-step tool-using AI agent"},
		{Command: "persona", Description: "Switch AI persona"},
		{Command: "memory", Description: "Manage long-term AI memory"},
		{Command: "remind", Description: "Set or list reminders"},
		{Command: "note", Description: "Save or view notes"},
		{Command: "task", Description: "Manage your todo list"},
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
		{Command: "fast", Description: "Fast low-latency AI response"},
		{Command: "race", Description: "Race multiple AI models concurrently"},
		{Command: "research", Description: "Web research with cited sources"},
		{Command: "summarize", Description: "Summarize recent group messages"},
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
