package main

import (
	"fmt"
	"log"
	"strings"

	"openrouter-bot/config"
	"openrouter-bot/ui"
	"openrouter-bot/user"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// hintInterval is how many requests pass between two model suggestions. It is
// deliberately sparse: a bot that nags is worse than one that stays quiet.
const hintInterval = 25

// maxHints caps how often a single user is ever nudged.
const maxHints = 3

// maybeSuggestModel tells a user whose pinned model scores worse than another
// available model. Users in automatic mode are never nudged: the routing has
// already picked the better model for them.
func (a *app) maybeSuggestModel(chatID int64, conf *config.Config, tracker *user.UsageTracker) {
	if !conf.SuggestModels {
		return
	}

	preference := tracker.Preference()
	if preference.Auto || preference.Model == "" {
		return
	}

	requests := tracker.RequestCount()
	if requests < hintInterval || requests%hintInterval != 0 {
		return
	}
	if tracker.HintsShown() >= maxHints {
		return
	}

	recommendation, err := a.chain.Recommend(tracker.UsageProfile())
	if err != nil {
		return
	}
	if recommendation.Model == "" || strings.EqualFold(recommendation.Model, preference.Model) {
		return
	}

	button, ok := ui.UseModelButton(recommendation.Model)
	if !ok {
		return
	}

	tracker.MarkHintShown()

	text := fmt.Sprintf(
		"💡 <b>%s</b> may fit your usage better than the pinned model.\n\nWhy: %s",
		escapeHTML(recommendation.Model),
		escapeHTML(recommendation.Reason),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(button),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✨ Stay on auto", ui.Encode(ui.ActionAuto)),
			tgbotapi.NewInlineKeyboardButtonData("🙅 No thanks", ui.Encode(ui.ActionNoop)),
		),
	)

	log.Printf("Suggesting %s to user %s (pinned %s)", recommendation.Model, tracker.UserID, preference.Model)

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.ReplyMarkup = keyboard

	if _, err := a.bot.Send(msg); err != nil {
		log.Printf("Failed to send model hint: %v", err)
	}
}
