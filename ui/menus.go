// Package ui builds the bot's buttons.
//
// Every keyboard in the bot is described here so that the callback data
// format lives in exactly one place: main.go and the streaming renderer both
// only call builders, and the parser in this file understands what the
// builders produced.
package ui

import (
	"fmt"
	"strconv"
	"strings"

	"openrouter-bot/provider"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Callback actions. Kept short: Telegram caps callback data at 64 bytes.
const (
	ActionMenu        = "menu"
	ActionHelp        = "help"
	ActionProviders   = "provs"
	ActionProvider    = "prov"
	ActionModels      = "models"
	ActionModel       = "model"
	ActionAuto        = "auto"
	ActionFavourites  = "favs"
	ActionFavourite   = "fav"
	ActionSettings    = "set"
	ActionToggle      = "tog"
	ActionReset       = "reset"
	ActionStats       = "stats"
	ActionRecommend   = "rec"
	ActionGroup       = "grp"
	ActionGroupAccess = "grpa"
	ActionGroupTrans  = "grpt"
	ActionUse         = "use"
	ActionRegenerate  = "regen"
	ActionFeedback    = "fb"
	ActionStop        = "stop"
	ActionNoop        = "noop"
)

// Callback is a decoded button press.
type Callback struct {
	Action string
	Args   []string
}

// parseLimit caps how many arguments a callback may carry. The data comes
// from a client, so it must never be trusted further than it is validated.
const parseLimit = 4

// Encode builds callback data for a raw action. Callers outside this package
// use it for the fixed buttons (menu, stats, ...) so the wire format stays in
// one place.
func Encode(action string, args ...string) string { return encode(action, args...) }

// UseModelButton offers "pin this model". Telegram caps callback data at 64
// bytes, so a model id that cannot fit returns ok=false and the caller simply
// omits the button instead of sending invalid data.
func UseModelButton(model string) (tgbotapi.InlineKeyboardButton, bool) {
	model = strings.TrimSpace(model)
	// Measure the payload itself: encode() degrades an oversized request to
	// "cb:noop", which would silently turn the button into a no-op instead of
	// hiding it.
	if model == "" || len("cb:"+ActionUse+":"+model) > 64 {
		return tgbotapi.InlineKeyboardButton{}, false
	}

	return button("✅ Use "+truncate(model, 30), ActionUse, model), true
}

// ModelFromCallback returns the model id carried by a "use" callback.
func ModelFromCallback(callback Callback) string {
	if callback.Action != ActionUse || len(callback.Args) != 1 {
		return ""
	}

	return strings.TrimSpace(callback.Args[0])
}

// ParseCallback decodes callback data produced by the builders below.
func ParseCallback(data string) (Callback, bool) {
	data = strings.TrimSpace(data)
	if !strings.HasPrefix(data, "cb:") {
		return Callback{}, false
	}

	parts := strings.Split(strings.TrimPrefix(data, "cb:"), ":")
	if len(parts) == 0 || parts[0] == "" || len(parts) > parseLimit {
		return Callback{}, false
	}

	return Callback{Action: parts[0], Args: parts[1:]}, true
}

// encode builds callback data and panics only on an internal mistake: every
// builder below is short enough to stay well under Telegram's 64 byte limit.
func encode(action string, args ...string) string {
	data := "cb:" + action
	for _, arg := range args {
		data += ":" + arg
	}
	if len(data) > 64 {
		return "cb:" + ActionNoop
	}

	return data
}

// button builds one inline button with encoded callback data.
func button(label, action string, args ...string) tgbotapi.InlineKeyboardButton {
	return tgbotapi.NewInlineKeyboardButtonData(label, encode(action, args...))
}

// indexArgs turns numbers into callback arguments.
func indexArgs(values ...int) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, strconv.Itoa(value))
	}

	return out
}

// ArgsToIndices converts callback arguments back into numbers, rejecting
// anything that is not a small non-negative integer. No arguments at all is a
// failure too: the callers always expect a value.
func ArgsToIndices(args ...string) ([]int, bool) {
	if len(args) == 0 {
		return nil, false
	}

	out := make([]int, 0, len(args))
	for _, arg := range args {
		value, err := strconv.Atoi(arg)
		if err != nil || value < 0 || value > 100000 {
			return nil, false
		}
		out = append(out, value)
	}

	return out, true
}

// -----------------------------------------------------------------------------
// MENUS
// -----------------------------------------------------------------------------

// MainMenu is the panel a user sees without typing a single command.
func MainMenu(settingsLabel string) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			button("🧠 Model", ActionProviders),
			button("⚙️ Settings", ActionSettings),
		),
		tgbotapi.NewInlineKeyboardRow(
			button("📊 My usage", ActionStats),
			button("🔮 Best for me", ActionRecommend),
		),
		tgbotapi.NewInlineKeyboardRow(
			button("🔄 New chat", ActionReset),
			button("⭐ Favourites", ActionFavourites),
		),
		tgbotapi.NewInlineKeyboardRow(
			button("❓ Help", ActionHelp),
		),
	)
}

// providerButtonLabel marks the active provider with a play arrow.
func providerButtonLabel(name string, active, configured bool) string {
	label := name
	if !configured {
		label += " (no key)"
	}
	if active {
		return "▶ " + label
	}

	return "   " + label
}

// ProviderList lets the user pick a provider, then a model inside it.
func ProviderList(infos []provider.Info, active string, page, pageSize int) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			button("✨ Auto — best for my usage", ActionAuto),
		),
	}

	start := page * pageSize
	end := start + pageSize
	if start > len(infos) {
		start = len(infos)
	}
	if end > len(infos) {
		end = len(infos)
	}

	for i := start; i < end; i++ {
		info := infos[i]
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			button(providerButtonLabel(info.Name, info.Name == active, info.Configured), ActionProvider, indexArgs(i)...),
		))
	}

	nav := Navigation(page, pageSize, len(infos), ActionProviders)
	if len(nav) > 0 {
		rows = append(rows, nav)
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(button("⬅️ Menu", ActionMenu)))

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// ModelList shows the models of one provider.
func ModelList(providerIndex int, providerName string, models []provider.ModelRef, current string, offset, pageSize int, favourite func(string) bool) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			button("▶ Use auto on "+truncate(providerName, 20), ActionProvider, indexArgs(providerIndex)...),
		),
	}

	end := offset + pageSize
	if offset > len(models) {
		offset = len(models)
	}
	if end > len(models) {
		end = len(models)
	}

	for i := offset; i < end; i++ {
		ref := models[i]

		label := ref.Model
		if ref.Current {
			label = "▶ " + label
		} else if favourite != nil && favourite(ref.Model) {
			label = "⭐ " + label
		}

		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			button(truncate(label, 44), ActionModel, indexArgs(providerIndex, offsetOf(models, ref.Model))...),
		))
	}

	if nav := Navigation(offset/pageSize, pageSize, len(models), ActionProvider, indexArgs(providerIndex)...); len(nav) > 0 {
		rows = append(rows, nav)
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		button("⬅️ Providers", ActionProviders),
		button("🏠 Menu", ActionMenu),
	))

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// offsetOf finds a model's index in the catalogue. The index is what travels
// in the callback data, so it must match the list the user is looking at.
func offsetOf(models []provider.ModelRef, model string) int {
	for i, ref := range models {
		if ref.Model == model {
			return i
		}
	}

	return 0
}

// Favourites lists bookmarked models, in catalogue order.
func Favourites(models []provider.ModelRef, favourite []string) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{}

	for i, ref := range models {
		for _, favouriteModel := range favourite {
			if !strings.EqualFold(ref.Model, favouriteModel) {
				continue
			}

			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				button(truncate(ref.Model, 44), ActionModel, indexArgs(i)...),
			))
		}
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		button("🧠 All models", ActionProviders),
		button("🏠 Menu", ActionMenu),
	))

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// Settings is the per user preference panel.
func Settings(s provider.Preference, showFooter, streaming bool, favourites int) tgbotapi.InlineKeyboardMarkup {
	autoLabel := "🌐 Auto model: OFF"
	if s.Auto {
		autoLabel = "🌐 Auto model: ON"
	}
	footerLabel := "📍 Show model: " + onOff(showFooter)
	streamLabel := "⚡ Live typing: " + onOff(streaming)

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			button(autoLabel, ActionToggle, "auto"),
		),
		tgbotapi.NewInlineKeyboardRow(
			button(footerLabel, ActionToggle, "footer"),
			button(streamLabel, ActionToggle, "stream"),
		),
		tgbotapi.NewInlineKeyboardRow(
			button("🧠 Model & provider", ActionProviders),
			button(fmt.Sprintf("⭐ Favourites (%d)", favourites), ActionFavourites),
		),
		tgbotapi.NewInlineKeyboardRow(
			button("🔄 New chat", ActionReset),
			button("🔮 Best for me", ActionRecommend),
		),
		tgbotapi.NewInlineKeyboardRow(
			button("📊 My usage", ActionStats),
			button("🏠 Menu", ActionMenu),
		),
	)
}

// GroupPanel is the panel shown by /group; only administrators can press the
// buttons that change the chat.
func GroupPanel(access string, translateEnabled bool, botIsAdmin bool) tgbotapi.InlineKeyboardMarkup {
	everyone := "👥 Everyone: " + mark(access == accessEveryone())
	admins := "🛡 Admins only: " + mark(access == accessAdmins())
	owner := "🔒 Owner only: " + mark(access == accessOwner())

	translate := "🌐 Auto translate: " + onOff(translateEnabled)

	note := "🛡 Bot is not admin here"
	if botIsAdmin {
		note = "🛡 Bot is admin here"
	}

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(button(everyone, ActionGroupAccess, accessEveryone())),
		tgbotapi.NewInlineKeyboardRow(button(admins, ActionGroupAccess, accessAdmins())),
		tgbotapi.NewInlineKeyboardRow(button(owner, ActionGroupAccess, accessOwner())),
		tgbotapi.NewInlineKeyboardRow(button(translate, ActionGroupTrans, "toggle")),
		tgbotapi.NewInlineKeyboardRow(button(note, ActionNoop)),
	)
}

// The access mode names match the groups package; they are duplicated as
// plain strings here so the UI package has no dependency on storage.
func accessEveryone() string { return "everyone" }

func accessAdmins() string { return "admins" }

func accessOwner() string { return "owner" }

// StopButton is attached to the placeholder while an answer streams.
func StopButton() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(button("🛑 Stop", ActionStop)),
	)
}

// AnswerActions is attached once an answer is complete: regenerate, switch
// model, and quick feedback. The model id is looked up by the caller from the
// pending answer, so nothing sensitive has to travel in the callback data.
func AnswerActions() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			button("🔁 Regenerate", ActionRegenerate),
			button("🧠 Model", ActionProviders),
		),
		tgbotapi.NewInlineKeyboardRow(
			button("👍", ActionFeedback, "up"),
			button("👎", ActionFeedback, "down"),
			button("⭐", ActionFavourite),
		),
	)
}

// FeedbackOnly replaces the action keyboard after a vote.
func FeedbackOnly(positive bool) tgbotapi.InlineKeyboardMarkup {
	label := "👍 Thanks!"
	if !positive {
		label = "👎 Noted, I'll avoid this model for you"
	}

	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(button(label, ActionNoop)),
		tgbotapi.NewInlineKeyboardRow(
			button("🔁 Regenerate", ActionRegenerate),
			button("🧠 Model", ActionProviders),
		),
	)
}

// Navigation renders prev / page indicator / next when it is needed.
func Navigation(page, pageSize, total int, action string, extra ...string) []tgbotapi.InlineKeyboardButton {
	if total <= pageSize {
		return nil
	}

	pages := (total + pageSize - 1) / pageSize

	var prev, next tgbotapi.InlineKeyboardButton

	prevArgs := append([]string{}, extra...)
	prevArgs = append(prevArgs, indexArgs(maxInt(page-1, 0))...)
	prev = button("◀️", action, prevArgs...)

	nextArgs := append([]string{}, extra...)
	nextArgs = append(nextArgs, indexArgs(minInt(page+1, pages-1))...)
	next = button("▶️", action, nextArgs...)

	if page <= 0 {
		prev = button("·", ActionNoop)
	}
	if page >= pages-1 {
		next = button("·", ActionNoop)
	}

	return tgbotapi.NewInlineKeyboardRow(
		prev,
		button(fmt.Sprintf("%d/%d", page+1, pages), ActionNoop),
		next,
	)
}

// -----------------------------------------------------------------------------
// SMALL HELPERS
// -----------------------------------------------------------------------------

func onOff(value bool) string {
	if value {
		return "ON"
	}

	return "OFF"
}

func mark(value bool) string {
	if value {
		return "✅"
	}

	return "▫️"
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit <= 1 {
		return string(runes[:limit])
	}

	return string(runes[:limit-1]) + "…"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}

	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}

	return b
}

// QuickKeyboard is the persistent reply keyboard, so the most common actions
// are one tap away without typing a command.
func QuickKeyboard() tgbotapi.ReplyKeyboardMarkup {
	return tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("🧠 Model"),
			tgbotapi.NewKeyboardButton("⚙️ Settings"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("📊 Usage"),
			tgbotapi.NewKeyboardButton("❓ Help"),
		),
	)
}

// HideKeyboard removes the persistent keyboard.
func HideKeyboard() tgbotapi.ReplyKeyboardRemove {
	return tgbotapi.NewRemoveKeyboard(false)
}
