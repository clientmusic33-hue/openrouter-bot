package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openrouter-bot/agent"
	"openrouter-bot/api"
	"openrouter-bot/config"
	"openrouter-bot/features/coding"
	"openrouter-bot/features/files"
	"openrouter-bot/features/memory"
	"openrouter-bot/features/personas"
	"openrouter-bot/features/reminders"
	"openrouter-bot/features/research"
	"openrouter-bot/features/voice"
	"openrouter-bot/internal/ratelimit"
	"openrouter-bot/lang"
	"openrouter-bot/provider"
	"openrouter-bot/router"
	"openrouter-bot/ui"
	"openrouter-bot/user"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/sashabaranov/go-openai"
)

// buildContextSupplement combines active persona instructions, coding assistant
// guidance, relevant long-term memory, and a compact summary of older history.
func (a *app) buildContextSupplement(
	chatID int64,
	prompt string,
	conf *config.Config,
	tracker *user.UsageTracker,
) string {
	var parts []string

	// 1. Group-level system prompt / persona overrides when in a group.
	if a.groups != nil {
		grp := a.groups.Get(chatID)
		if grp.SystemPrompt != "" {
			parts = append(parts, grp.SystemPrompt)
		}
		if grp.Persona != "" {
			if p, ok := personas.Lookup(grp.Persona); ok && p.Prompt != "" {
				parts = append(parts, p.Prompt)
			}
		}
	}

	// 2. User-selected persona.
	if tracker != nil {
		pKey, customPrompt := tracker.Persona()
		if pKey == "custom" && customPrompt != "" {
			parts = append(parts, "Persona instructions: "+customPrompt)
		} else if p, ok := personas.Lookup(pKey); ok && p.Prompt != "" {
			parts = append(parts, p.Prompt)
		}
	}

	// 3. Coding assistant task detection.
	if codeAnalysis := coding.Analyze(prompt); codeAnalysis.IsCoding && codeAnalysis.Supplement != "" {
		parts = append(parts, codeAnalysis.Supplement)
	}

	// 4. Long-term memory (unless disabled for user or group).
	groupMemDisabled := a.groups != nil && a.groups.Get(chatID).MemoryDisabled
	if a.memory != nil && tracker != nil && !groupMemDisabled {
		_, _ = a.memory.MaybeExtractImplicitFact(tracker.UserID, prompt)
		if memCtx := a.memory.RelevantContext(tracker.UserID, prompt, 6); memCtx != "" {
			parts = append(parts, memCtx)
		}
	}

	// 5. Short-term history token optimization & compact summary.
	if a.optimizer != nil && tracker != nil && conf != nil {
		convKey := fmt.Sprintf("%d:%s", chatID, tracker.UserID)
		_, summary := a.optimizer.Optimize(convKey, tracker.GetMessages(), memory.ContextConfig{
			MaxContextMessages: conf.MaxContextMessages,
			MaxContextTokens:   conf.MaxContextTokens,
			SummaryThreshold:   conf.SummaryThreshold,
		})
		if summary != "" {
			parts = append(parts, summary)
		}
	}

	return strings.Join(parts, "\n\n")
}

// deliverReminder sends a due reminder notification to its Telegram chat.
func (a *app) deliverReminder(r reminders.Reminder) {
	if a == nil || a.bot == nil {
		return
	}
	text := fmt.Sprintf("⏰ <b>Reminder</b>\n\n%s", escapeHTML(r.Text))
	a.send(r.ChatID, text, "HTML")
}

// handleFast runs a prompt using the lowest-latency model route (or races when
// multiple fast providers are available).
func (a *app) handleFast(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	promptText := strings.TrimSpace(message.CommandArguments())
	if promptText == "" {
		a.send(message.Chat.ID, "⚡ <b>Fast mode</b>\n\nUsage: <code>/fast &lt;question&gt;</code>\nRoutes to the fastest low-latency model immediately.", "HTML")
		return
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		if !tracker.HaveAccess(conf) {
			a.send(message.Chat.ID, lang.Translate("budget_out", conf.Lang), "")
			return
		}
		if err := tracker.AllowRequest(conf); err != nil {
			a.send(message.Chat.ID, lang.Translate("rate_limit", conf.Lang), "")
			return
		}

		done := a.metrics.BeginRequest()
		opts := a.chatOptions(conf, tracker)
		opts.TaskOverride = router.TaskFast
		opts.SystemSupplement = a.buildContextSupplement(message.Chat.ID, promptText, conf, tracker)

		res, err := api.Generate(a.ctx, a.bot, a.chain, api.Prompt{
			ChatID:  message.Chat.ID,
			Text:    promptText,
			Message: message,
		}, conf, tracker, opts)
		done(res.Model, memory.EstimateTokens(promptText+res.Text), err)
		if len(res.Messages) > 0 && res.Text != "" && !res.Stopped {
			a.rememberAnswer(message.Chat.ID, res.Messages[0], senderID(message), promptText, res.Model)
		}
	}()
}

// handleRace races 2-3 healthy models concurrently and returns the first valid
// completion (or synthesises if prefixed with "judge ").
func (a *app) handleRace(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	args := strings.TrimSpace(message.CommandArguments())
	if args == "" {
		a.send(message.Chat.ID,
			"🏎 <b>Model Race Mode</b>\n\n"+
				"<code>/race &lt;question&gt;</code> — race 2–3 healthy models concurrently and return the fastest answer\n"+
				"<code>/race judge &lt;question&gt;</code> — race models and synthesise the strongest combined answer",
			"HTML",
		)
		return
	}

	synthesize := false
	lower := strings.ToLower(args)
	if strings.HasPrefix(lower, "judge ") || strings.HasPrefix(lower, "synth ") {
		synthesize = true
		parts := strings.SplitN(args, " ", 2)
		if len(parts) == 2 {
			args = strings.TrimSpace(parts[1])
		}
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		if !tracker.HaveAccess(conf) {
			a.send(message.Chat.ID, lang.Translate("budget_out", conf.Lang), "")
			return
		}
		if err := tracker.AllowRequest(conf); err != nil {
			a.send(message.Chat.ID, lang.Translate("rate_limit", conf.Lang), "")
			return
		}

		done := a.metrics.BeginRequest()
		opts := a.chatOptions(conf, tracker)
		opts.SystemSupplement = a.buildContextSupplement(message.Chat.ID, args, conf, tracker)

		res, err := api.GenerateRace(a.ctx, a.bot, a.chain, api.Prompt{
			ChatID:  message.Chat.ID,
			Text:    args,
			Message: message,
		}, conf, tracker, opts, synthesize)
		done(res.Model, memory.EstimateTokens(args+res.Text), err)
		if len(res.Messages) > 0 && res.Text != "" {
			a.rememberAnswer(message.Chat.ID, res.Messages[0], senderID(message), args, res.Model)
		}
	}()
}

// handleAgentCommand runs the multi-step tool-using agent on a task.
func (a *app) handleAgentCommand(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	task := strings.TrimSpace(message.CommandArguments())
	if task == "" {
		a.send(message.Chat.ID,
			"🤖 <b>AI Agent</b>\n\n"+
				"Usage: <code>/agent &lt;task&gt;</code>\n\n"+
				"The agent plans steps and uses built-in tools (calculator, time, web search, URL reader, GitHub lookup, translator, notes) up to "+
				fmt.Sprintf("%d steps.", conf.MaxAgentSteps),
			"HTML",
		)
		return
	}

	if a.limiter != nil {
		if err := a.limiter.Allow(tracker.UserID, tracker.GetUserRole(conf), ratelimit.CategoryAgent, conf.RateLimitPerMinute); err != nil {
			a.send(message.Chat.ID, "⏳ "+err.Error(), "")
			return
		}
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		if !tracker.HaveAccess(conf) {
			a.send(message.Chat.ID, lang.Translate("budget_out", conf.Lang), "")
			return
		}

		placeholder, err := a.bot.Send(tgbotapi.NewMessage(message.Chat.ID, "🤖 Agent planning and executing tools..."))
		if err != nil {
			return
		}

		done := a.metrics.BeginRequest()
		ag := a.agent
		if ag == nil {
			ag = agent.New(a.chain, nil)
		}

		state, runErr := ag.Run(a.ctx, task, agent.Config{
			MaxSteps: conf.MaxAgentSteps,
			Timeout:  conf.RequestTimeout,
			UserID:   tracker.UserID,
			Role:     tracker.GetUserRole(conf),
		})
		if runErr != nil {
			done("", 0, runErr)
			edit := tgbotapi.NewEditMessageText(message.Chat.ID, placeholder.MessageID, "❌ Agent task failed: "+runErr.Error())
			_, _ = a.bot.Send(edit)
			return
		}

		var out strings.Builder
		if len(state.Steps) > 0 {
			out.WriteString(fmt.Sprintf("🛠 Steps executed: %d\n\n", len(state.Steps)))
		}
		out.WriteString(state.FinalAnswer)
		if tracker.Settings().ShowFooter && state.Model != "" {
			out.WriteString(fmt.Sprintf("\n\n⚡ %s · %s", state.Provider, state.Model))
		}

		done(state.Model, memory.EstimateTokens(task+state.FinalAnswer), nil)
		edit := tgbotapi.NewEditMessageText(message.Chat.ID, placeholder.MessageID, out.String())
		if _, err := a.bot.Send(edit); err != nil {
			a.send(message.Chat.ID, out.String(), "")
		}
	}()
}

// handleResearchCommand runs web research on a topic with source citations.
func (a *app) handleResearchCommand(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	topic := strings.TrimSpace(message.CommandArguments())
	if topic == "" {
		a.send(message.Chat.ID,
			"🔬 <b>Web Research</b>\n\n"+
				"Usage: <code>/research &lt;topic or URL&gt;</code>\n"+
				"Searches, extracts, deduplicates sources, and produces a cited summary.",
			"HTML",
		)
		return
	}

	if a.limiter != nil {
		if err := a.limiter.Allow(tracker.UserID, tracker.GetUserRole(conf), ratelimit.CategoryResearch, conf.RateLimitPerMinute); err != nil {
			a.send(message.Chat.ID, "⏳ "+err.Error(), "")
			return
		}
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		if !tracker.HaveAccess(conf) {
			a.send(message.Chat.ID, lang.Translate("budget_out", conf.Lang), "")
			return
		}

		placeholder, err := a.bot.Send(tgbotapi.NewMessage(message.Chat.ID, "🔎 Researching sources..."))
		if err != nil {
			return
		}

		done := a.metrics.BeginRequest()
		ctx, cancel := context.WithTimeout(a.ctx, conf.RequestTimeout)
		defer cancel()

		report, err := research.Research(ctx, a.chain, topic, nil)
		if err != nil {
			done("", 0, err)
			edit := tgbotapi.NewEditMessageText(message.Chat.ID, placeholder.MessageID, "❌ Research failed. Please try again.")
			_, _ = a.bot.Send(edit)
			return
		}

		var sb strings.Builder
		sb.WriteString("🔬 Research: " + report.Topic + "\n\n")
		sb.WriteString(report.Summary)
		if len(report.Sources) > 0 && !strings.Contains(report.Summary, "http") {
			sb.WriteString("\n\nSources:\n")
			for i, src := range report.Sources {
				sb.WriteString(fmt.Sprintf("[%d] %s — %s\n", i+1, src.Title, src.URL))
			}
		}
		if tracker.Settings().ShowFooter && report.Model != "" {
			sb.WriteString(fmt.Sprintf("\n\n⚡ %s · %s", report.Provider, report.Model))
		}

		done(report.Model, memory.EstimateTokens(topic+report.Summary), nil)
		text := strings.TrimSpace(sb.String())
		edit := tgbotapi.NewEditMessageText(message.Chat.ID, placeholder.MessageID, text)
		if _, err := a.bot.Send(edit); err != nil {
			a.send(message.Chat.ID, text, "")
		}
	}()
}

// handleMemoryCommand manages long-term user memory (/memory, /memory list,
// /memory forget, /memory clear, /memory off, /memory on, /memory add).
func (a *app) handleMemoryCommand(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	chatID := message.Chat.ID
	if a.memory == nil {
		a.send(chatID, "❌ Memory manager is not available.", "")
		return
	}

	args := strings.TrimSpace(message.CommandArguments())
	fields := strings.Fields(args)
	sub := ""
	rest := ""
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
		rest = strings.TrimSpace(strings.TrimPrefix(args, fields[0]))
	}

	userID := tracker.UserID

	switch sub {
	case "", "list":
		a.sendScreen(chatID, a.memoryScreen(tracker))

	case "off", "disable":
		_ = a.memory.SetEnabled(userID, false)
		a.send(chatID, "🛑 Long-term memory disabled. I will not save or use persistent facts.", "")

	case "on", "enable":
		_ = a.memory.SetEnabled(userID, true)
		a.send(chatID, "🟢 Long-term memory enabled.", "")

	case "clear":
		_ = a.memory.Clear(userID)
		a.send(chatID, "🧹 Long-term memory cleared.", "")

	case "forget", "del", "delete", "rm":
		if rest == "" {
			a.send(chatID, "Usage: <code>/memory forget &lt;id or keyword&gt;</code>", "HTML")
			return
		}
		removed, err := a.memory.Forget(userID, rest)
		if err != nil {
			a.send(chatID, "❌ "+err.Error(), "")
			return
		}
		a.send(chatID, fmt.Sprintf("🗑 Removed %d memory item(s).", removed), "")

	case "add", "save", "remember":
		if rest == "" {
			a.send(chatID, "Usage: <code>/memory add &lt;fact to remember&gt;</code>", "HTML")
			return
		}
		fact, err := a.memory.Add(userID, rest)
		if err != nil {
			a.send(chatID, "❌ "+err.Error(), "")
			return
		}
		a.send(chatID, fmt.Sprintf("🧠 Saved memory #%d: %s", fact.ID, escapeHTML(fact.Text)), "HTML")

	default:
		// Treat `/memory <fact>` directly as adding a fact.
		fact, err := a.memory.Add(userID, args)
		if err != nil {
			a.send(chatID, "❌ "+err.Error(), "")
			return
		}
		a.send(chatID, fmt.Sprintf("🧠 Saved memory #%d: %s", fact.ID, escapeHTML(fact.Text)), "HTML")
	}
}

// handlePersonaCommand handles /persona, /persona list, /persona <name>, and
// /persona custom <prompt>.
func (a *app) handlePersonaCommand(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	chatID := message.Chat.ID
	args := strings.TrimSpace(message.CommandArguments())

	if args == "" || strings.EqualFold(args, "list") {
		a.sendScreen(chatID, a.personasScreen(tracker))
		return
	}

	lower := strings.ToLower(args)
	if strings.HasPrefix(lower, "custom ") {
		customPrompt := strings.TrimSpace(args[len("custom "):])
		if customPrompt == "" {
			a.send(chatID, "❌ Provide a prompt after /persona custom.", "")
			return
		}
		tracker.SetPersona("custom", customPrompt)
		a.send(chatID, "🎭 Custom persona activated.", "")
		return
	}

	p, ok := personas.Lookup(args)
	if !ok {
		var names []string
		for _, item := range personas.List() {
			names = append(names, item.Key)
		}
		a.send(chatID, fmt.Sprintf(
			"❌ Unknown persona <code>%s</code>\n\nAvailable: %s\nOr use <code>/persona custom &lt;instructions&gt;</code>",
			escapeHTML(args), escapeHTML(strings.Join(names, ", ")),
		), "HTML")
		return
	}

	tracker.SetPersona(p.Key, "")
	a.send(chatID, fmt.Sprintf("%s Persona set to <b>%s</b> — %s", p.Emoji, escapeHTML(p.Name), escapeHTML(p.Description)), "HTML")
}

// handleSummarizeCommand summarises recent messages in a group chat and
// extracts action items.
func (a *app) handleSummarizeCommand(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	chatID := message.Chat.ID

	if !isGroupChat(message.Chat) {
		// In a private chat, summarize the user's current conversation history.
		history := tracker.GetMessages()
		if len(history) == 0 {
			a.send(chatID, "📝 No conversation messages to summarize yet.", "")
			return
		}
		var sb strings.Builder
		for _, m := range history {
			sb.WriteString(fmt.Sprintf("%s: %s\n", m.Role, m.Content))
		}
		a.runSummaryCompletion(chatID, sb.String(), conf)
		return
	}

	recent := a.groups.RecentMessages(chatID, 50)
	if len(recent) == 0 {
		a.send(chatID, "📝 Not enough recent group messages to summarize yet.", "")
		return
	}

	var transcript strings.Builder
	for _, m := range recent {
		transcript.WriteString(fmt.Sprintf("[%s] %s: %s\n", m.At.Format("15:04"), m.Sender, m.Text))
	}

	a.runSummaryCompletion(chatID, transcript.String(), conf)
}

func (a *app) runSummaryCompletion(chatID int64, transcript string, conf *config.Config) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		ctx, cancel := context.WithTimeout(a.ctx, conf.RequestTimeout)
		defer cancel()

		candidates, _ := router.Route(a.chain, router.RouteRequest{
			Prompt:       transcript,
			TaskOverride: router.TaskSummarization,
			Preference:   provider.Preference{Auto: true},
		})
		if len(candidates) == 0 {
			a.send(chatID, "❌ No model available to summarize right now.", "")
			return
		}

		req := openai.ChatCompletionRequest{
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: "You summarize conversations concisely in plain text. Provide: 1) Key Discussion Points, 2) Decisions Made, and 3) Action Items (with owners when mentioned).",
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: "Summarize the following conversation and extract action items:\n\n" + transcript,
				},
			},
			Temperature: 0.2,
			MaxTokens:   1000,
		}

		for _, cand := range candidates {
			resp, err := cand.Complete(ctx, req)
			if err != nil {
				a.chain.RecordFailure(cand.Provider, err)
				continue
			}
			if len(resp.Choices) > 0 && strings.TrimSpace(resp.Choices[0].Message.Content) != "" {
				a.chain.RecordSuccess(cand.Provider)
				a.send(chatID, "📋 <b>Conversation Summary &amp; Action Items</b>\n\n"+escapeHTML(strings.TrimSpace(resp.Choices[0].Message.Content)), "HTML")
				return
			}
		}

		a.send(chatID, "❌ Could not generate summary right now.", "")
	}()
}

// handleRemindCommand schedules or deletes a persistent reminder.
func (a *app) handleRemindCommand(message *tgbotapi.Message, tracker *user.UsageTracker) {
	chatID := message.Chat.ID
	if a.reminders == nil {
		a.send(chatID, "❌ Reminder service is not available.", "")
		return
	}

	args := strings.TrimSpace(message.CommandArguments())
	if args == "" || strings.EqualFold(args, "list") {
		a.handleRemindersListCommand(message, tracker)
		return
	}

	fields := strings.Fields(args)
	if len(fields) == 2 && (strings.EqualFold(fields[0], "del") || strings.EqualFold(fields[0], "delete") || strings.EqualFold(fields[0], "cancel")) {
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			a.send(chatID, "❌ Provide a valid reminder number.", "")
			return
		}
		removed, _ := a.reminders.DeleteReminder(tracker.UserID, id)
		if !removed {
			a.send(chatID, fmt.Sprintf("❌ Reminder #%d not found.", id), "")
			return
		}
		a.send(chatID, fmt.Sprintf("🗑 Cancelled reminder #%d.", id), "")
		return
	}

	dueAt, text, err := reminders.ParseRemindCommand(time.Now(), args)
	if err != nil {
		a.send(chatID, "⏰ "+err.Error(), "")
		return
	}

	r, err := a.reminders.AddReminder(tracker.UserID, chatID, dueAt, text)
	if err != nil {
		a.send(chatID, "❌ "+err.Error(), "")
		return
	}

	until := time.Until(r.DueAt).Round(time.Second)
	a.send(chatID, fmt.Sprintf("⏰ Reminder #%d set for <b>%s</b> (in %s):\n%s",
		r.ID, r.DueAt.Format("15:04:05 MST"), until, escapeHTML(r.Text)), "HTML")
}

// handleRemindersListCommand lists pending reminders for the user.
func (a *app) handleRemindersListCommand(message *tgbotapi.Message, tracker *user.UsageTracker) {
	chatID := message.Chat.ID
	if a.reminders == nil {
		a.send(chatID, "❌ Reminder service is not available.", "")
		return
	}

	list := a.reminders.ListReminders(tracker.UserID)
	if len(list) == 0 {
		a.send(chatID, "⏰ <b>Reminders</b>\n\nNo active reminders.\nSet one with <code>/remind 30m Check build</code>", "HTML")
		return
	}

	var sb strings.Builder
	sb.WriteString("⏰ <b>Active Reminders</b>\n\n")
	for _, r := range list {
		until := time.Until(r.DueAt).Round(time.Second)
		sb.WriteString(fmt.Sprintf("#%d · in %s — %s\n", r.ID, until, escapeHTML(r.Text)))
	}
	sb.WriteString("\nCancel with <code>/remind del &lt;id&gt;</code>")
	a.send(chatID, sb.String(), "HTML")
}

// handleNoteCommand adds or deletes a persistent note.
func (a *app) handleNoteCommand(message *tgbotapi.Message, tracker *user.UsageTracker) {
	chatID := message.Chat.ID
	if a.reminders == nil {
		a.send(chatID, "❌ Notes service is not available.", "")
		return
	}

	args := strings.TrimSpace(message.CommandArguments())
	if args == "" || strings.EqualFold(args, "list") {
		a.handleNotesListCommand(message, tracker)
		return
	}

	if strings.EqualFold(args, "clear") {
		_ = a.reminders.ClearNotes(tracker.UserID)
		a.send(chatID, "🧹 All notes cleared.", "")
		return
	}

	fields := strings.Fields(args)
	if len(fields) == 2 && (strings.EqualFold(fields[0], "del") || strings.EqualFold(fields[0], "delete") || strings.EqualFold(fields[0], "rm")) {
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			a.send(chatID, "❌ Provide a valid note ID.", "")
			return
		}
		removed, _ := a.reminders.DeleteNote(tracker.UserID, id)
		if !removed {
			a.send(chatID, fmt.Sprintf("❌ Note #%d not found.", id), "")
			return
		}
		a.send(chatID, fmt.Sprintf("🗑 Deleted note #%d.", id), "")
		return
	}

	if strings.HasPrefix(strings.ToLower(args), "add ") {
		args = strings.TrimSpace(args[4:])
	}

	note, err := a.reminders.AddNote(tracker.UserID, args)
	if err != nil {
		a.send(chatID, "❌ "+err.Error(), "")
		return
	}
	a.send(chatID, fmt.Sprintf("📝 Saved note #%d:\n%s", note.ID, escapeHTML(note.Text)), "HTML")
}

// handleNotesListCommand displays all saved notes for the user.
func (a *app) handleNotesListCommand(message *tgbotapi.Message, tracker *user.UsageTracker) {
	chatID := message.Chat.ID
	if a.reminders == nil {
		a.send(chatID, "❌ Notes service is not available.", "")
		return
	}

	notes := a.reminders.ListNotes(tracker.UserID)
	if len(notes) == 0 {
		a.send(chatID, "📝 <b>Notes</b>\n\nNo saved notes yet.\nSave one with <code>/note &lt;text&gt;</code>", "HTML")
		return
	}

	var sb strings.Builder
	sb.WriteString("📝 <b>Your Notes</b>\n\n")
	for _, n := range notes {
		sb.WriteString(fmt.Sprintf("#%d · %s\n", n.ID, escapeHTML(n.Text)))
	}
	sb.WriteString("\nDelete with <code>/note del &lt;id&gt;</code> or <code>/note clear</code>")
	a.send(chatID, sb.String(), "HTML")
}

// handleTaskCommand manages persistent user tasks (/task add, /task done, /task del).
func (a *app) handleTaskCommand(message *tgbotapi.Message, tracker *user.UsageTracker) {
	chatID := message.Chat.ID
	if a.reminders == nil {
		a.send(chatID, "❌ Tasks service is not available.", "")
		return
	}

	args := strings.TrimSpace(message.CommandArguments())
	if args == "" || strings.EqualFold(args, "list") {
		a.handleTasksListCommand(message, tracker)
		return
	}

	fields := strings.Fields(args)
	sub := strings.ToLower(fields[0])

	switch sub {
	case "done", "complete", "check":
		if len(fields) < 2 {
			a.send(chatID, "Usage: <code>/task done &lt;id&gt;</code>", "HTML")
			return
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			a.send(chatID, "❌ Provide a valid task ID.", "")
			return
		}
		ok, _ := a.reminders.CompleteTask(tracker.UserID, id)
		if !ok {
			a.send(chatID, fmt.Sprintf("❌ Task #%d not found.", id), "")
			return
		}
		a.send(chatID, fmt.Sprintf("✅ Task #%d marked complete!", id), "")

	case "del", "delete", "rm":
		if len(fields) < 2 {
			a.send(chatID, "Usage: <code>/task del &lt;id&gt;</code>", "HTML")
			return
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			a.send(chatID, "❌ Provide a valid task ID.", "")
			return
		}
		ok, _ := a.reminders.DeleteTask(tracker.UserID, id)
		if !ok {
			a.send(chatID, fmt.Sprintf("❌ Task #%d not found.", id), "")
			return
		}
		a.send(chatID, fmt.Sprintf("🗑 Deleted task #%d.", id), "")

	default:
		text := args
		if sub == "add" {
			text = strings.TrimSpace(strings.TrimPrefix(args, fields[0]))
		}
		task, err := a.reminders.AddTask(tracker.UserID, text)
		if err != nil {
			a.send(chatID, "❌ "+err.Error(), "")
			return
		}
		a.send(chatID, fmt.Sprintf("☑️ Added task #%d: %s", task.ID, escapeHTML(task.Text)), "HTML")
	}
}

// handleTasksListCommand displays the user's todo list.
func (a *app) handleTasksListCommand(message *tgbotapi.Message, tracker *user.UsageTracker) {
	chatID := message.Chat.ID
	if a.reminders == nil {
		a.send(chatID, "❌ Tasks service is not available.", "")
		return
	}

	tasks := a.reminders.ListTasks(tracker.UserID)
	if len(tasks) == 0 {
		a.send(chatID, "☑️ <b>Tasks</b>\n\nNo tasks yet.\nAdd one with <code>/task &lt;description&gt;</code>", "HTML")
		return
	}

	var sb strings.Builder
	sb.WriteString("☑️ <b>Your Tasks</b>\n\n")
	for _, t := range tasks {
		box := "⬜"
		if t.Done {
			box = "✅"
		}
		sb.WriteString(fmt.Sprintf("%s #%d · %s\n", box, t.ID, escapeHTML(t.Text)))
	}
	sb.WriteString("\nComplete with <code>/task done &lt;id&gt;</code> · Delete with <code>/task del &lt;id&gt;</code>")
	a.send(chatID, sb.String(), "HTML")
}

// handleCodeCommand routes explicit coding commands (/code, /review, /explain, /testgen)
// to a coding-capable model with specialized instructions.
func (a *app) handleCodeCommand(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	cmd := strings.ToLower(message.Command())
	args := strings.TrimSpace(message.CommandArguments())
	if args == "" && message.ReplyToMessage != nil {
		args = strings.TrimSpace(messageText(message.ReplyToMessage))
	}
	if args == "" {
		a.send(message.Chat.ID, fmt.Sprintf("💻 Usage: <code>/%s &lt;code or question&gt;</code> (or reply to a message containing code).", escapeHTML(cmd)), "HTML")
		return
	}

	prefix := ""
	switch cmd {
	case "review":
		prefix = "Perform a thorough code review (correctness, concurrency, security, performance, maintainability):\n\n"
	case "explain":
		prefix = "Explain this code or error step by step and identify any bugs:\n\n"
	case "testgen":
		prefix = "Generate comprehensive unit tests (covering happy paths and edge cases) for:\n\n"
	}
	fullPrompt := prefix + args

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		if !tracker.HaveAccess(conf) {
			a.send(message.Chat.ID, lang.Translate("budget_out", conf.Lang), "")
			return
		}
		if err := tracker.AllowRequest(conf); err != nil {
			a.send(message.Chat.ID, lang.Translate("rate_limit", conf.Lang), "")
			return
		}

		done := a.metrics.BeginRequest()
		opts := a.chatOptions(conf, tracker)
		opts.TaskOverride = router.TaskCoding
		opts.SystemSupplement = a.buildContextSupplement(message.Chat.ID, fullPrompt, conf, tracker)

		res, err := api.Generate(a.ctx, a.bot, a.chain, api.Prompt{
			ChatID:  message.Chat.ID,
			Text:    fullPrompt,
			Message: message,
		}, conf, tracker, opts)
		done(res.Model, memory.EstimateTokens(fullPrompt+res.Text), err)
		if len(res.Messages) > 0 && res.Text != "" && !res.Stopped {
			a.rememberAnswer(message.Chat.ID, res.Messages[0], senderID(message), fullPrompt, res.Model)
		}
	}()
}

// handleDocument downloads and analyses an uploaded document (TXT, PDF, DOCX,
// CSV, JSON, source code) within MAX_FILE_SIZE.
func (a *app) handleDocument(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	if message == nil || message.Document == nil {
		return
	}
	doc := message.Document
	chatID := message.Chat.ID

	maxBytes := conf.MaxFileSize
	if maxBytes <= 0 {
		maxBytes = files.DefaultMaxFileSize
	}
	if int64(doc.FileSize) > maxBytes {
		a.send(chatID, fmt.Sprintf("❌ File is too large (%d MB). Maximum allowed size is %d MB.", int64(doc.FileSize)>>20, maxBytes>>20), "")
		return
	}

	kind, _ := files.DetectKind(doc.FileName, doc.MimeType)
	if kind == files.KindUnknown {
		a.send(chatID, "❌ Unsupported file format. Supported: TXT, MD, PDF, DOCX, CSV, JSON, YAML, and source code files.", "")
		return
	}

	if a.limiter != nil {
		if err := a.limiter.Allow(tracker.UserID, tracker.GetUserRole(conf), ratelimit.CategoryFile, conf.RateLimitPerMinute); err != nil {
			a.send(chatID, "⏳ "+err.Error(), "")
			return
		}
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		if !tracker.HaveAccess(conf) {
			a.send(chatID, lang.Translate("budget_out", conf.Lang), "")
			return
		}

		tgFile, err := a.bot.GetFile(tgbotapi.FileConfig{FileID: doc.FileID})
		if err != nil {
			log.Printf("Failed to resolve Telegram file %s: %v", doc.FileID, err)
			a.send(chatID, "❌ Could not download file from Telegram.", "")
			return
		}

		client := provider.SharedHTTPClient(30 * time.Second)
		req, err := http.NewRequestWithContext(a.ctx, http.MethodGet, tgFile.Link(a.bot.Token), nil)
		if err != nil {
			a.send(chatID, "❌ Could not download file.", "")
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			a.send(chatID, "❌ Failed to download file.", "")
			return
		}
		defer resp.Body.Close()

		extracted, err := files.Process(doc.FileName, doc.MimeType, resp.Body, maxBytes)
		if err != nil {
			a.send(chatID, "❌ File processing error: "+err.Error(), "")
			return
		}

		promptText := files.BuildPrompt(extracted, message.Caption)
		done := a.metrics.BeginRequest()
		opts := a.chatOptions(conf, tracker)
		if extracted.Kind == files.KindCode {
			opts.TaskOverride = router.TaskCoding
		} else {
			opts.TaskOverride = router.TaskSummarization
		}
		opts.SystemSupplement = a.buildContextSupplement(chatID, promptText, conf, tracker)

		res, err := api.Generate(a.ctx, a.bot, a.chain, api.Prompt{
			ChatID: chatID,
			Text:   promptText,
		}, conf, tracker, opts)
		done(res.Model, memory.EstimateTokens(promptText+res.Text), err)
		if len(res.Messages) > 0 && res.Text != "" && !res.Stopped {
			a.rememberAnswer(chatID, res.Messages[0], senderID(message), promptText, res.Model)
		}
	}()
}

// handleVoice transcribes a Telegram voice/audio message when a speech-to-text
// backend is available and answers the transcribed prompt.
func (a *app) handleVoice(message *tgbotapi.Message, conf *config.Config, tracker *user.UsageTracker) {
	if message == nil {
		return
	}
	chatID := message.Chat.ID

	fileID := ""
	filename := "voice.ogg"
	fileSize := 0
	if message.Voice != nil {
		fileID = message.Voice.FileID
		fileSize = message.Voice.FileSize
	} else if message.Audio != nil {
		fileID = message.Audio.FileID
		fileSize = message.Audio.FileSize
		if message.Audio.FileName != "" {
			filename = message.Audio.FileName
		}
	}
	if fileID == "" {
		return
	}

	if !voice.Available(a.chain) {
		if !isGroupChat(message.Chat) {
			a.send(chatID, "🎙 Voice transcription is not configured on this bot (set GROQ_API_KEY or VOICE_STT_API_KEY to enable speech-to-text).", "")
		}
		return
	}

	if fileSize > 15<<20 {
		a.send(chatID, "❌ Voice message is too large (max 15 MB).", "")
		return
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case a.semaphore <- struct{}{}:
			defer func() { <-a.semaphore }()
		case <-a.ctx.Done():
			return
		}

		if !tracker.HaveAccess(conf) {
			a.send(chatID, lang.Translate("budget_out", conf.Lang), "")
			return
		}

		tgFile, err := a.bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
		if err != nil {
			a.send(chatID, "❌ Could not retrieve voice message.", "")
			return
		}

		client := provider.SharedHTTPClient(30 * time.Second)
		req, err := http.NewRequestWithContext(a.ctx, http.MethodGet, tgFile.Link(a.bot.Token), nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			a.send(chatID, "❌ Failed to download voice message.", "")
			return
		}
		defer resp.Body.Close()

		audioBytes, err := io.ReadAll(io.LimitReader(resp.Body, 15<<20))
		if err != nil {
			a.send(chatID, "❌ Could not read voice audio.", "")
			return
		}

		ctx, cancel := context.WithTimeout(a.ctx, 35*time.Second)
		transcript, err := voice.Transcribe(ctx, a.chain, bytes.Clone(audioBytes), filename)
		cancel()
		if err != nil {
			if !errors.Is(err, voice.ErrVoiceUnavailable) {
				a.send(chatID, "❌ Voice transcription failed: "+err.Error(), "")
			}
			return
		}

		a.send(chatID, "🎙 <i>"+escapeHTML(transcript)+"</i>", "HTML")

		done := a.metrics.BeginRequest()
		opts := a.chatOptions(conf, tracker)
		opts.SystemSupplement = a.buildContextSupplement(chatID, transcript, conf, tracker)

		res, err := api.Generate(a.ctx, a.bot, a.chain, api.Prompt{
			ChatID: chatID,
			Text:   transcript,
		}, conf, tracker, opts)
		done(res.Model, memory.EstimateTokens(transcript+res.Text), err)
		if len(res.Messages) > 0 && res.Text != "" && !res.Stopped {
			a.rememberAnswer(chatID, res.Messages[0], senderID(message), transcript, res.Model)
		}
	}()
}

// personasScreen renders the inline persona selector.
func (a *app) personasScreen(tracker *user.UsageTracker) screen {
	currentKey, customPrompt := tracker.Persona()

	var lines []string
	var rows [][]tgbotapi.InlineKeyboardButton

	list := personas.List()
	for i := 0; i < len(list); i += 2 {
		var row []tgbotapi.InlineKeyboardButton
		for j := i; j < i+2 && j < len(list); j++ {
			p := list[j]
			marker := ""
			if strings.EqualFold(p.Key, currentKey) {
				marker = " ✅"
			}
			lines = append(lines, fmt.Sprintf("%s <b>%s</b>%s — %s", p.Emoji, escapeHTML(p.Name), marker, escapeHTML(p.Description)))
			btnLabel := fmt.Sprintf("%s %s%s", p.Emoji, p.Name, marker)
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(btnLabel, ui.Encode(ui.ActionPersona, p.Key)))
		}
		rows = append(rows, row)
	}

	if currentKey == "custom" && customPrompt != "" {
		lines = append(lines, "\n✨ <b>Custom:</b> "+escapeHTML(customPrompt))
	}

	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Menu", ui.Encode(ui.ActionMenu)),
	))

	return screen{
		Text: "🎭 <b>AI Personas</b>\n\n" +
			strings.Join(lines, "\n") +
			"\n\nTap a persona below or use <code>/persona custom &lt;prompt&gt;</code>.",
		Keyboard: tgbotapi.NewInlineKeyboardMarkup(rows...),
	}
}

// memoryScreen renders the user's long-term memory status and stored facts.
func (a *app) memoryScreen(tracker *user.UsageTracker) screen {
	enabled := true
	var facts []memory.Fact
	if a.memory != nil {
		enabled = a.memory.Enabled(tracker.UserID)
		facts = a.memory.List(tracker.UserID)
	}

	status := "🟢 enabled"
	if !enabled {
		status = "🔴 disabled"
	}

	var sb strings.Builder
	sb.WriteString("🗂 <b>Long-Term Memory</b>\n\n")
	sb.WriteString(fmt.Sprintf("Status: <b>%s</b>\n", status))
	sb.WriteString(fmt.Sprintf("Short-term history: <b>%d</b> message(s)\n\n", len(tracker.GetMessages())))

	if len(facts) == 0 {
		sb.WriteString("No saved facts yet.\nUse <code>/memory add &lt;fact&gt;</code> or say <i>\"Remember that I...\"</i>\n")
	} else {
		sb.WriteString("<b>Saved Facts:</b>\n")
		for _, f := range facts {
			sb.WriteString(fmt.Sprintf("#%d · %s\n", f.ID, escapeHTML(f.Text)))
		}
	}

	sb.WriteString("\nCommands: <code>/memory add</code> · <code>/memory forget &lt;id&gt;</code> · <code>/memory clear</code> · <code>/memory off</code>")

	toggleArg := "mem_off"
	toggleLabel := "🛑 Disable memory"
	if !enabled {
		toggleArg = "mem_on"
		toggleLabel = "🟢 Enable memory"
	}

	return screen{
		Text: sb.String(),
		Keyboard: tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(toggleLabel, ui.Encode(ui.ActionToggle, toggleArg)),
				tgbotapi.NewInlineKeyboardButtonData("🔄 Clear chat", ui.Encode(ui.ActionReset)),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("⬅️ Menu", ui.Encode(ui.ActionMenu)),
			),
		),
	}
}

// remindersScreen renders a combined overview of reminders, tasks, and notes.
func (a *app) remindersScreen(tracker *user.UsageTracker) screen {
	var rems []reminders.Reminder
	var tasks []reminders.Task
	var notes []reminders.Note
	if a.reminders != nil {
		rems = a.reminders.ListReminders(tracker.UserID)
		tasks = a.reminders.ListTasks(tracker.UserID)
		notes = a.reminders.ListNotes(tracker.UserID)
	}

	text := fmt.Sprintf(
		"⏰ <b>Reminders, Tasks &amp; Notes</b>\n\n"+
			"Active reminders: <b>%d</b>\n"+
			"Tasks: <b>%d</b>\n"+
			"Saved notes: <b>%d</b>\n\n"+
			"<b>Quick commands:</b>\n"+
			"• <code>/remind 30m Check server</code>\n"+
			"• <code>/reminders</code>\n"+
			"• <code>/task Add unit tests</code>\n"+
			"• <code>/tasks</code>\n"+
			"• <code>/note Key project idea</code>\n"+
			"• <code>/notes</code>",
		len(rems), len(tasks), len(notes),
	)

	return screen{
		Text: text,
		Keyboard: tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("⬅️ Menu", ui.Encode(ui.ActionMenu)),
			),
		),
	}
}
