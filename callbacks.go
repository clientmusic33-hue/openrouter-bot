package main

import (
	"log"
	"strings"

	"openrouter-bot/api"
	"openrouter-bot/config"
	"openrouter-bot/ui"
	"openrouter-bot/user"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// handleCallback serves every inline button in the bot.
func (a *app) handleCallback(query *tgbotapi.CallbackQuery, conf *config.Config) {
	if query == nil || query.Message == nil || query.From == nil {
		return
	}

	// Telegram keeps a spinner on the button until the query is answered, so
	// every path has to answer it. The deferred call covers the early returns.
	answered := false
	defer func() {
		if !answered {
			a.toast(query, "")
		}
	}()

	chat := query.Message.Chat
	chatID := chat.ID
	messageID := query.Message.MessageID

	tracker := a.users.GetUser(chatID, query.From.ID, query.From.UserName, conf)

	callback, ok := ui.ParseCallback(query.Data)
	if !ok {
		return
	}

	// A restricted group must not leak its panels to every member, but the
	// owner and the admins keep their controls.
	if !a.mayUse(chat, query.From.ID, conf) {
		a.editScreen(chatID, messageID, screen{
			Text: "🛡 The bot is restricted in this group.\nAsk an admin to run /group.",
		})
		return
	}

	switch callback.Action {

	case ui.ActionNoop:
		return

	case ui.ActionMenu:
		a.editScreen(chatID, messageID, a.homeScreen(conf, tracker))

	case ui.ActionHelp:
		a.editScreen(chatID, messageID, screen{
			Text: helpText(isGroupChat(chat)),
			Keyboard: tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🏠 Menu", ui.Encode(ui.ActionMenu)),
				),
			),
		})

	case ui.ActionProviders:
		page := firstIndex(callback.Args, 0)
		a.editScreen(chatID, messageID, a.providersScreen(conf, tracker, page))

	case ui.ActionProvider:
		index := firstIndex(callback.Args, 0)
		page := secondIndex(callback.Args, 0)
		a.editScreen(chatID, messageID, a.modelsScreen(conf, tracker, index, page))

	case ui.ActionModel:
		providerIndex := firstIndex(callback.Args, -1)
		modelIndex := secondIndex(callback.Args, -1)
		model := a.modelAt(providerIndex, modelIndex)
		if model == "" {
			a.toast(query, "That model is gone, try again")
			return
		}
		providerName, _ := a.chain.ProviderOf(model)
		tracker.SetModel(model, providerName)
		a.toast(query, "Pinned "+model+" ✅")
		a.editScreen(chatID, messageID, a.modelsScreen(conf, tracker, providerIndex, 0))

	case ui.ActionUse:
		model := ui.ModelFromCallback(callback)
		if model == "" {
			return
		}
		providerName, _ := a.chain.ProviderOf(model)
		tracker.SetModel(model, providerName)
		a.toast(query, "Pinned "+model+" ✅")
		a.editScreen(chatID, messageID, a.settingsScreen(conf, tracker))

	case ui.ActionAuto:
		tracker.ResetPreference()
		a.toast(query, "Automatic mode on ✨")
		a.editScreen(chatID, messageID, a.providersScreen(conf, tracker, 0))

	case ui.ActionSettings:
		a.editScreen(chatID, messageID, a.settingsScreen(conf, tracker))

	case ui.ActionToggle:
		switch firstArg(callback.Args, "") {
		case "auto":
			settings := tracker.Settings()
			tracker.SetAuto(!settings.Auto)
			if !settings.Auto {
				tracker.SetProvider("")
			}
		case "footer":
			tracker.SetShowFooter(!tracker.Settings().ShowFooter)
		case "stream":
			tracker.SetStreaming(!tracker.Settings().Streaming)
		}
		a.editScreen(chatID, messageID, a.settingsScreen(conf, tracker))

	case ui.ActionReset:
		tracker.ClearHistory()
		a.editScreen(chatID, messageID, screen{
			Text: "🧠 New chat started. Memory cleared — ask me anything. ✨",
			Keyboard: tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🏠 Menu", ui.Encode(ui.ActionMenu)),
				),
			),
		})

	case ui.ActionStats:
		a.editScreen(chatID, messageID, a.statsScreen(conf, tracker))

	case ui.ActionRecommend:
		a.editScreen(chatID, messageID, a.recommendScreen(conf, tracker))

	case ui.ActionFavourites:
		a.editScreen(chatID, messageID, a.favouritesScreen(conf, tracker))

	case ui.ActionFavourite:
		answer, ok := a.pendingAnswer(chatID, messageID)
		if !ok || answer.Model == "" {
			a.toast(query, "Open a model list to bookmark one")
			return
		}
		if tracker.ToggleFavourite(answer.Model) {
			a.toast(query, "Added to favourites ⭐")
		} else {
			a.toast(query, "Removed from favourites")
		}

	case ui.ActionGroup:
		a.groupCallback(query, conf)

	case ui.ActionGroupAccess:
		a.groupAccessCallback(query, conf, callback)

	case ui.ActionGroupTrans:
		a.groupTranslateCallback(query, conf)

	case ui.ActionRegenerate:
		a.regenerateCallback(query, conf, tracker, messageID)

	case ui.ActionFeedback:
		a.feedbackCallback(query, callback, tracker, messageID)

	case ui.ActionStop:
		if tracker.StopStream() {
			a.toast(query, "Stopped 🛑")
		} else {
			a.toast(query, "Nothing is running")
		}

	default:
		log.Printf("Unhandled callback action %q", callback.Action)
	}
}

// -----------------------------------------------------------------------------
// CALLBACK IMPLEMENTATIONS
// -----------------------------------------------------------------------------

// groupCallback is a shortcut from a chat to its panel.
func (a *app) groupCallback(query *tgbotapi.CallbackQuery, conf *config.Config) {
	if !isGroupChat(query.Message.Chat) {
		a.toast(query, "Group panel is only available in groups")
		return
	}
	if !a.isChatAdmin(query.Message.Chat, query.From.ID, conf) {
		a.toast(query, "Only group admins can change this")
		return
	}

	a.editScreen(query.Message.Chat.ID, query.Message.MessageID, a.groupScreen(conf, query.Message.Chat))
}

// groupAccessCallback changes who may use the bot in a chat.
func (a *app) groupAccessCallback(query *tgbotapi.CallbackQuery, conf *config.Config, callback ui.Callback) {
	mode := firstArg(callback.Args, "")
	if !isGroupChat(query.Message.Chat) || !config.ValidGroupAccess(mode) {
		return
	}
	if !a.isChatAdmin(query.Message.Chat, query.From.ID, conf) {
		a.toast(query, "Only group admins can change this")
		return
	}

	if err := a.groups.SetAccess(query.Message.Chat.ID, mode); err != nil {
		log.Printf("Failed to set access for chat %d: %v", query.Message.Chat.ID, err)
		a.toast(query, "Could not save that")
		return
	}

	a.toast(query, "Saved: "+accessLabel(mode))
	a.editScreen(query.Message.Chat.ID, query.Message.MessageID, a.groupScreen(conf, query.Message.Chat))
}

// groupTranslateCallback toggles automatic translation.
func (a *app) groupTranslateCallback(query *tgbotapi.CallbackQuery, conf *config.Config) {
	if !isGroupChat(query.Message.Chat) {
		return
	}
	if !a.isChatAdmin(query.Message.Chat, query.From.ID, conf) {
		a.toast(query, "Only group admins can change this")
		return
	}

	chatID := query.Message.Chat.ID
	enabled := !a.groups.Get(chatID).TranslateEnabled

	if err := a.groups.SetEnabled(chatID, enabled); err != nil {
		log.Printf("Failed to toggle translation for chat %d: %v", chatID, err)
		a.toast(query, "Could not save that")
		return
	}

	if enabled {
		a.toast(query, "Auto translation on 🌐")
	} else {
		a.toast(query, "Auto translation off")
	}

	a.editScreen(chatID, query.Message.MessageID, a.groupScreen(conf, query.Message.Chat))
}

// regenerateCallback replays the question behind an answer.
func (a *app) regenerateCallback(query *tgbotapi.CallbackQuery, conf *config.Config, tracker *user.UsageTracker, messageID int) {
	chatID := query.Message.Chat.ID

	answer, ok := a.pendingAnswer(chatID, messageID)
	if !ok || strings.TrimSpace(answer.Prompt) == "" {
		a.toast(query, "Too old to regenerate — just ask again 🙂")
		return
	}

	a.toast(query, "Regenerating 🔁")
	a.forgetAnswer(chatID, messageID)

	// Replace the previous answer instead of stacking a second one on top of
	// it: the history loses the old assistant text (and the user copy of the
	// prompt, which Generate adds again).
	tracker.DropLastAssistant()
	tracker.DropLastUser()

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		result, err := api.Generate(a.ctx, a.bot, a.chain, api.Prompt{
			ChatID: chatID,
			Text:   answer.Prompt,
		}, conf, tracker, a.chatOptions(conf, tracker))
		if err != nil {
			log.Printf("Regeneration failed for user %s: %v", tracker.UserID, err)
			return
		}

		if len(result.Messages) > 0 && result.Text != "" {
			a.rememberAnswer(chatID, result.Messages[0], query.From.ID, answer.Prompt, result.Model)
		}
	}()
}

// feedbackCallback records a thumbs up or down for the model that answered.
func (a *app) feedbackCallback(query *tgbotapi.CallbackQuery, callback ui.Callback, tracker *user.UsageTracker, messageID int) {
	chatID := query.Message.Chat.ID
	positive := firstArg(callback.Args, "up") == "up"

	answer, ok := a.pendingAnswer(chatID, messageID)
	if !ok || answer.Model == "" {
		a.toast(query, "Noted 👍")
		return
	}

	tracker.Feedback(answer.Model, positive)

	if positive {
		a.toast(query, "Thanks! 👍")
	} else {
		a.toast(query, "Noted — I'll avoid this model for you 👎")
	}

	keyboard := ui.FeedbackOnly(positive)
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, keyboard)
	if _, err := a.bot.Send(edit); err != nil && !strings.Contains(err.Error(), "message is not modified") {
		log.Printf("Failed to update feedback keyboard: %v", err)
	}
}

// -----------------------------------------------------------------------------
// CALLBACK HELPERS
// -----------------------------------------------------------------------------

// toast answers a callback query, optionally with a short notification.
func (a *app) toast(query *tgbotapi.CallbackQuery, text string) {
	config := tgbotapi.NewCallback(query.ID, truncateCallbackText(text))
	if _, err := a.bot.Request(config); err != nil {
		log.Printf("Failed to answer callback: %v", err)
	}
}

// truncateCallbackText keeps notifications short: Telegram rejects them above
// 200 characters.
func truncateCallbackText(text string) string {
	runes := []rune(text)
	if len(runes) <= 180 {
		return text
	}

	return string(runes[:180]) + "…"
}

func firstArg(args []string, fallback string) string {
	if len(args) > 0 {
		return args[0]
	}

	return fallback
}

// firstIndex reads the first callback argument as a number.
func firstIndex(args []string, fallback int) int {
	indices, ok := ui.ArgsToIndices(args...)
	if !ok || len(indices) < 1 {
		return fallback
	}

	return indices[0]
}

// secondIndex reads the second callback argument as a number.
func secondIndex(args []string, fallback int) int {
	indices, ok := ui.ArgsToIndices(args...)
	if !ok || len(indices) < 2 {
		return fallback
	}

	return indices[1]
}

// modelAt resolves a (provider, model) index pair - the pair the model
// buttons carry - into a model id.
func (a *app) modelAt(providerIndex, modelIndex int) string {
	if _, models := a.modelsForProvider(providerIndex); modelIndex >= 0 && modelIndex < len(models) {
		return models[modelIndex].Model
	}

	return ""
}

// helpText is shared by /help and the help button.
func helpText(group bool) string {
	text := "❓ <b>How to use me</b>\n\n" +
		"Just send a message — I answer with the best available model. ✨\n\n" +
		"<b>Buttons</b>\n" +
		"🧠 Model — pick a provider, then a model, or stay on auto\n" +
		"⚙️ Settings — your preferences\n" +
		"📊 Usage — your statistics\n" +
		"🔮 Best for me — a model suggestion for your usage\n\n" +
		"<b>Commands</b>\n" +
		"/menu · /model · /provider · /auto · /reset · /stop\n\n" +
		"Everything is free, no limits. 🤝"

	if group {
		text += "\n\nIn groups I answer when you mention me or reply to me."
	}

	return text
}
