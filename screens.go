package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"openrouter-bot/config"
	"openrouter-bot/provider"
	"openrouter-bot/ui"
	"openrouter-bot/user"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// A screen is one message: its text and the buttons below it. Screens are
// shared by the slash commands and by the callbacks so that a screen looks the
// same however it was opened.
type screen struct {
	Text     string `json:"text"`
	Keyboard tgbotapi.InlineKeyboardMarkup
	// Quick attaches the persistent reply keyboard instead of the inline one.
	Quick bool
}

const (
	providersPageSize = 6
	modelsPageSize    = 6
)

// homeScreen is the panel a user gets from /start, /menu or a button.
func (a *app) homeScreen(conf *config.Config, tracker *user.UsageTracker) screen {
	settings := tracker.Settings()
	mode := "✨ auto — the bot picks the best model for you"
	if !settings.Auto && settings.Model != "" {
		mode = "📌 pinned: <code>" + escapeHTML(settings.Model) + "</code>"
	} else if settings.Provider != "" {
		mode = "✨ auto on <b>" + escapeHTML(settings.Provider) + "</b>"
	}

	text := "🤖 <b>AI assistant</b>\n\n" +
		"Model: " + mode + "\n" +
		fmt.Sprintf("Providers online: <b>%d</b>\n\n", len(a.chain.UsableNames())) +
		"Just send me a message — or use the buttons below. ✨"

	return screen{
		Text:     text,
		Keyboard: ui.MainMenu(tracker.SettingsSummary()),
		Quick:    true,
	}
}

// providersScreen lists the provider chain, health included.
func (a *app) providersScreen(conf *config.Config, tracker *user.UsageTracker, page int) screen {
	status := a.chain.Status()
	active := a.chain.ActiveName()

	rows := make([]string, 0, len(status))
	for _, info := range status {
		icon := "🟢 available"
		switch {
		case !info.Configured:
			icon = "🔴 unavailable"
		case info.Cooldown > 0:
			icon = "🟡 degraded"
		case info.Failures > 0:
			icon = "🟡 degraded"
		case info.Local:
			icon = "🏠 local"
		}

		line := fmt.Sprintf("<b>%s</b> · %s · %d model(s)", escapeHTML(info.Name), icon, len(info.Models))
		if info.AvgLatency > 0 {
			line += fmt.Sprintf(" · ⚡ %dms", info.AvgLatency.Milliseconds())
		}
		if info.Cooldown > 0 {
			line += fmt.Sprintf(" · cooling down %s", info.Cooldown)
		}
		if !info.Configured {
			line += " · no API key"
		}
		rows = append(rows, line)
	}

	text := "🧠 <b>Model &amp; provider</b>\n\n" +
		"Your choice: <b>" + escapeHTML(tracker.SettingsSummary()) + "</b>\n\n" +
		strings.Join(rows, "\n") + "\n\n" +
		"Tap a provider to browse its models."

	if page < 0 {
		page = 0
	}

	return screen{
		Text:     text,
		Keyboard: ui.ProviderList(status, active, page, providersPageSize),
	}
}

// modelsForProvider returns the provider name and its models in catalogue
// order. The index into the returned slice is what the model buttons carry in
// their callback data, so both the renderer and the handler use this.
func (a *app) modelsForProvider(providerIndex int) (string, []provider.ModelRef) {
	names := a.chain.Names()
	if providerIndex < 0 || providerIndex >= len(names) {
		return "", nil
	}

	providerName := names[providerIndex]
	catalogue := a.chain.ModelCatalog()

	models := make([]provider.ModelRef, 0, len(catalogue))
	for _, ref := range catalogue {
		if strings.EqualFold(ref.Provider, providerName) {
			models = append(models, ref)
		}
	}

	return providerName, models
}

// modelsScreen lists one provider's models.
func (a *app) modelsScreen(conf *config.Config, tracker *user.UsageTracker, providerIndex, page int) screen {
	providerName, models := a.modelsForProvider(providerIndex)
	if providerName == "" {
		return a.providersScreen(conf, tracker, 0)
	}

	if page < 0 {
		page = 0
	}

	offset := page * modelsPageSize
	end := offset + modelsPageSize
	if end > len(models) {
		end = len(models)
	}

	var modelLines []string
	if offset < len(models) {
		for _, ref := range models[offset:end] {
			info := provider.LookupModel(ref.Model)
			var badges []string
			if info.Fast {
				badges = append(badges, "⚡ fast")
			}
			if info.Reasoning {
				badges = append(badges, "🧠 reasoning")
			}
			if info.Vision {
				badges = append(badges, "👁 vision")
			}
			if info.Coding {
				badges = append(badges, "💻 coding")
			}
			badgeStr := ""
			if len(badges) > 0 {
				badgeStr = " · " + strings.Join(badges, " · ")
			}
			modelLines = append(modelLines, fmt.Sprintf("• <code>%s</code>%s", escapeHTML(ref.Model), badgeStr))
		}
	}

	details := ""
	if len(modelLines) > 0 {
		details = "\n\n" + strings.Join(modelLines, "\n")
	}

	text := fmt.Sprintf(
		"🧠 <b>%s</b> · %d model(s)\n\n"+
			"Tap a model to pin it, or keep <b>✨ auto</b> and let the bot choose.\n"+
			"Current: <b>%s</b>%s",
		escapeHTML(providerName), len(models), escapeHTML(tracker.SettingsSummary()), details,
	)

	// The marker follows the model this user actually uses, not the
	// bot-wide default, so a pin is visible in the list it was made from.
	current := tracker.Preference().Model
	if current == "" {
		current = a.chain.Model()
	}

	return screen{
		Text: text,
		Keyboard: ui.ModelList(
			providerIndex,
			providerName,
			models,
			current,
			page*modelsPageSize,
			modelsPageSize,
			tracker.IsFavourite,
		),
	}
}

// settingsScreen is the per user preference panel.
func (a *app) settingsScreen(conf *config.Config, tracker *user.UsageTracker) screen {
	settings := tracker.Settings()

	text := "⚙️ <b>Settings</b>\n\n" +
		"Model: <b>" + escapeHTML(tracker.SettingsSummary()) + "</b>\n" +
		"Show which model answered: <b>" + onOff(settings.ShowFooter) + "</b>\n" +
		"Live typing: <b>" + onOff(settings.Streaming) + "</b>\n\n" +
		"These settings apply to you only."

	return screen{
		Text: text,
		Keyboard: ui.Settings(
			tracker.Preference(),
			settings.ShowFooter,
			settings.Streaming,
			len(settings.Favourites),
		),
	}
}

// statsScreen summarises what the bot knows about the user.
func (a *app) statsScreen(conf *config.Config, tracker *user.UsageTracker) screen {
	tracker.CheckHistory(conf.MaxHistorySize, conf.MaxHistoryTime)

	requests := tracker.RequestCount()
	history := len(tracker.GetMessages())

	text := fmt.Sprintf(
		"📊 <b>Your usage</b>\n\n"+
			"Requests: <b>%d</b>\n"+
			"Messages in memory: <b>%d</b>\n"+
			"Model: <b>%s</b>\n"+
			"Mode: <b>%s</b>\n",
		requests,
		history,
		escapeHTML(tracker.SettingsSummary()),
		modeLabel(conf),
	)

	if tracker.CanViewStats(conf) {
		text += fmt.Sprintf(
			"\nToday: <b>$%s</b>\nThis month: <b>$%s</b>\nAll time: <b>$%s</b>",
			formatMoney(tracker.GetCurrentCost("daily")),
			formatMoney(tracker.GetCurrentCost("monthly")),
			formatMoney(tracker.GetCurrentCost("total")),
		)
	}

	return screen{
		Text: text,
		Keyboard: tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🔄 New chat", ui.Encode(ui.ActionReset)),
				tgbotapi.NewInlineKeyboardButtonData("🏠 Menu", ui.Encode(ui.ActionMenu)),
			),
		),
	}
}

// recommendScreen explains which model fits the user's usage.
func (a *app) recommendScreen(conf *config.Config, tracker *user.UsageTracker) screen {
	profile := tracker.UsageProfile()

	recommendation, err := a.chain.Recommend(profile)
	if err != nil {
		return screen{Text: "😕 No model available to recommend right now."}
	}

	history := "no history yet"
	if profile.HistoryMessages > 0 {
		history = fmt.Sprintf("%d messages in memory", profile.HistoryMessages)
	}

	vision := "no"
	if profile.UsesVision {
		vision = "yes"
	}

	text := fmt.Sprintf(
		"🔮 <b>Best model for you</b>\n\n"+
			"<code>%s</code>\nvia <b>%s</b>\n\n"+
			"Why: %s\n\n"+
			"Your usage: ~%.0f requests/day · avg prompt %.0f chars · %s · images: %s",
		escapeHTML(recommendation.Model),
		escapeHTML(recommendation.Provider),
		escapeHTML(recommendation.Reason),
		profile.RequestsPerDay,
		profile.AvgPromptChars,
		history,
		vision,
	)

	rows := [][]tgbotapi.InlineKeyboardButton{}
	if button, ok := ui.UseModelButton(recommendation.Model); ok {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(button))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("🧠 All models", ui.Encode(ui.ActionProviders)),
		tgbotapi.NewInlineKeyboardButtonData("🏠 Menu", ui.Encode(ui.ActionMenu)),
	))

	return screen{Text: text, Keyboard: tgbotapi.NewInlineKeyboardMarkup(rows...)}
}

// favouritesScreen lists the user's bookmarked models.
func (a *app) favouritesScreen(conf *config.Config, tracker *user.UsageTracker) screen {
	favourites := tracker.Settings().Favourites
	catalogue := a.chain.ModelCatalog()

	if len(favourites) == 0 {
		return screen{
			Text: "⭐ <b>Favourites</b>\n\nNothing here yet.\nOpen a model list and tap ⭐ under an answer to bookmark one.",
			Keyboard: tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData("🧠 All models", ui.Encode(ui.ActionProviders)),
					tgbotapi.NewInlineKeyboardButtonData("🏠 Menu", ui.Encode(ui.ActionMenu)),
				),
			),
		}
	}

	var lines []string
	for _, ref := range catalogue {
		for _, favourite := range favourites {
			if strings.EqualFold(ref.Model, favourite) {
				lines = append(lines, "⭐ <code>"+escapeHTML(ref.Model)+"</code> · "+escapeHTML(ref.Provider))
			}
		}
	}
	if len(lines) == 0 {
		for _, favourite := range favourites {
			lines = append(lines, "⭐ <code>"+escapeHTML(favourite)+"</code>")
		}
	}

	return screen{
		Text:     "⭐ <b>Favourites</b>\n\n" + strings.Join(lines, "\n"),
		Keyboard: ui.Favourites(catalogue, favourites),
	}
}

// groupScreen is the chat panel; only admins reach it.
func (a *app) groupScreen(conf *config.Config, chat *tgbotapi.Chat) screen {
	settings := a.groups.Get(chat.ID)
	access := a.accessMode(chat.ID, conf)

	text := "🛡 <b>Group panel</b>\n\n" +
		"Who can use me here: <b>" + accessLabel(access) + "</b>\n" +
		"Auto translate: <b>" + onOff(settings.TranslateEnabled) + "</b>" +
		" (" + escapeHTML(settings.TargetLanguage) + ")\n" +
		"Bot is admin: <b>" + onOff(a.groups.BotIsAdmin(chat.ID)) + "</b>\n\n" +
		"Admins can change these; the bot owner always has access."

	return screen{
		Text:     text,
		Keyboard: ui.GroupPanel(access, settings.TranslateEnabled, a.groups.BotIsAdmin(chat.ID)),
	}
}

// adminScreen is the owner panel.
func (a *app) adminScreen(conf *config.Config) screen {
	var builder strings.Builder

	snap := a.metrics.Snapshot()
	queueUsed := len(a.semaphore)
	queueCap := cap(a.semaphore)

	storageBackend := "json"
	if a.store != nil {
		storageBackend = a.store.Type()
	}

	var cacheHits, cacheMisses int64
	var cacheEntries int
	if a.cache != nil {
		cacheEntries, cacheHits, cacheMisses = a.cache.Stats()
	}

	builder.WriteString("👑 <b>Admin Observability Panel</b>\n\n")
	builder.WriteString(fmt.Sprintf("Uptime: <b>%s</b>\n", time.Since(a.started).Round(time.Second)))
	builder.WriteString(fmt.Sprintf("Active requests: <b>%d</b> (queue %d/%d)\n", snap.ActiveRequests, queueUsed, queueCap))
	builder.WriteString(fmt.Sprintf("Total requests: <b>%d</b> · Errors: <b>%d</b> (%.1f%%)\n", snap.TotalRequests, snap.ErrorCount, snap.ErrorRatePct))
	builder.WriteString(fmt.Sprintf("Est. tokens used: <b>%d</b> · Cost: <b>$%s</b>\n", snap.TokensUsed, formatMoney(snap.TotalCostUSD)))
	builder.WriteString(fmt.Sprintf("Users seen: <b>%d</b> · Groups: <b>%d</b>\n", a.users.ActiveUsers(), a.groups.Count()))
	builder.WriteString(fmt.Sprintf("Memory: <b>%.1f MB</b> · Goroutines: <b>%d</b>\n", snap.HeapAllocMB, snap.Goroutines))
	builder.WriteString(fmt.Sprintf("Storage: <b>%s</b> · Cache: <b>%d entries</b> (hits %d / miss %d)\n", escapeHTML(storageBackend), cacheEntries, cacheHits, cacheMisses))
	builder.WriteString(fmt.Sprintf("Default model: <code>%s</code>\n", escapeHTML(a.chain.Model())))

	builder.WriteString("\n<b>Providers &amp; Latency</b>\n")
	for _, info := range a.chain.Status() {
		icon := "🟢 available"
		switch {
		case !info.Configured:
			icon = "🔴 unavailable"
		case info.Cooldown > 0 || info.CircuitState == "open":
			icon = "🟡 degraded"
		case info.Failures > 0:
			icon = "🟡 degraded"
		}

		lat := "—"
		if info.AvgLatency > 0 {
			lat = fmt.Sprintf("%dms", info.AvgLatency.Milliseconds())
		}

		builder.WriteString(fmt.Sprintf(
			"• <b>%s</b> (%s) · ok %d · fail %d · to %d · lat %s\n",
			escapeHTML(info.Name), icon, info.Successes, info.Failures, info.Timeouts, lat,
		))
	}

	if len(snap.TopModels) > 0 {
		builder.WriteString("\n<b>Top Models Used</b>\n")
		for _, m := range snap.TopModels {
			builder.WriteString(fmt.Sprintf("• <code>%s</code>: <b>%d</b> req\n", escapeHTML(m.Model), m.Count))
		}
	}

	return screen{
		Text: builder.String(),
		Keyboard: tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🌐 Providers", ui.Encode(ui.ActionProviders)),
				tgbotapi.NewInlineKeyboardButtonData("🏠 Menu", ui.Encode(ui.ActionMenu)),
			),
		),
	}
}

// -----------------------------------------------------------------------------
// SMALL HELPERS
// -----------------------------------------------------------------------------

func onOff(value bool) string {
	if value {
		return "ON ✅"
	}

	return "OFF"
}

func accessLabel(mode string) string {
	switch mode {
	case config.AccessAdmins:
		return "admins only"
	case config.AccessOwner:
		return "owner only"
	default:
		return "everyone"
	}
}

func modeLabel(conf *config.Config) string {
	if conf.PublicMode {
		return "public · free to use"
	}

	return conf.GroupChatMode
}

// sendScreen delivers a screen as a new message.
func (a *app) sendScreen(chatID int64, s screen) {
	if s.Quick {
		msg := tgbotapi.NewMessage(chatID, s.Text)
		msg.ParseMode = tgbotapi.ModeHTML
		msg.ReplyMarkup = ui.QuickKeyboard()

		if _, err := a.bot.Send(msg); err != nil {
			logSendFailure(chatID, err)
		}
		return
	}

	msg := tgbotapi.NewMessage(chatID, s.Text)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.ReplyMarkup = s.Keyboard

	if _, err := a.bot.Send(msg); err != nil {
		logSendFailure(chatID, err)
	}
}

// editScreen rewrites an existing message, which is what keeps the menus from
// piling up in the chat.
func (a *app) editScreen(chatID int64, messageID int, s screen) {
	edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, s.Text, s.Keyboard)
	edit.ParseMode = tgbotapi.ModeHTML

	if _, err := a.bot.Send(edit); err != nil {
		if !strings.Contains(err.Error(), "message is not modified") {
			logSendFailure(chatID, err)
		}
	}
}

func logSendFailure(chatID int64, err error) {
	log.Printf("Failed to send screen to chat %d: %v", chatID, err)
}
