package api

import (
	"log"
	"strings"
	"sync"
	"time"

	"openrouter-bot/config"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// editInterval throttles Telegram edits while streaming. Telegram API calls
// are relatively expensive; 350ms feels responsive without hammering it.
const editInterval = 350 * time.Millisecond

// maxChunks caps how many Telegram messages a single answer may occupy.
const maxChunks = 20

// maxChunksDefault is used when SplitMessage is called without a cap.
const maxChunksDefault = 20

// typingInterval is how often the "typing…" chat action is refreshed.
// Telegram clears it after about five seconds.
const typingInterval = 5 * time.Second

// -----------------------------------------------------------------------------
// CHUNKED MESSAGE RENDERING
// -----------------------------------------------------------------------------

// messageSink renders a growing answer across as many Telegram messages as it
// needs. Telegram caps a single message at 4096 characters, and that limit is
// enforced on edits too, so a long answer has to spill into new messages.
type messageSink struct {
	bot        *tgbotapi.BotAPI
	chatID     int64
	messageIDs []int
	rendered   []string
	limit      int
	maxChunks  int
	parseMode  string
}

func newMessageSink(bot *tgbotapi.BotAPI, chatID int64, firstMessageID, limit int) *messageSink {
	if limit <= 0 {
		limit = config.ChunkLimit
	}

	return &messageSink{
		bot:        bot,
		chatID:     chatID,
		messageIDs: []int{firstMessageID},
		rendered:   []string{""},
		limit:      limit,
		maxChunks:  maxChunks,
	}
}

// Messages returns the ids of the messages that carry the answer.
func (s *messageSink) Messages() []int {
	return s.messageIDs
}

// Update renders text, sending extra messages when it outgrows the current
// ones. It reports whether anything changed.
func (s *messageSink) Update(text string) bool {
	chunks := SplitMessage(text, s.limit, s.maxChunks)
	if len(chunks) == 0 {
		return false
	}

	changed := false

	for i, chunk := range chunks {
		if i >= len(s.messageIDs) {
			msg := tgbotapi.NewMessage(s.chatID, chunk)
			sent, err := s.bot.Send(msg)
			if err != nil {
				log.Printf("Failed to send continuation message: %v", err)
				return changed
			}
			s.messageIDs = append(s.messageIDs, sent.MessageID)
			s.rendered = append(s.rendered, chunk)
			changed = true

			continue
		}

		if chunk == s.rendered[i] {
			continue
		}

		if editMessage(s.bot, s.chatID, s.messageIDs[i], chunk, "") {
			s.rendered[i] = chunk
			changed = true
		}
	}

	return changed
}

// Finish writes the final text. When markdown is enabled it upgrades the
// formatting, trying MarkdownV2 and then legacy Markdown before settling for
// the plain text Update already wrote.
func (s *messageSink) Finish(text string, markdown bool) {
	s.Update(text)

	if !markdown {
		return
	}

	chunks := SplitMessage(text, s.limit, s.maxChunks)

	for i, chunk := range chunks {
		if i >= len(s.messageIDs) {
			return
		}

		// Markdown is only applied at the end: a half-written "**bold" would
		// be rejected by Telegram while streaming.
		if editMessage(s.bot, s.chatID, s.messageIDs[i], chunk, tgbotapi.ModeMarkdownV2) {
			continue
		}
		if editMessage(s.bot, s.chatID, s.messageIDs[i], chunk, tgbotapi.ModeMarkdown) {
			continue
		}

		// Leave the plain text version that Update already wrote.
		log.Printf("Markdown rendering failed for chunk %d, keeping plain text", i)
	}
}

// SetKeyboard attaches a keyboard to the first message of the answer. The
// buttons live on the placeholder, which is the message the user is looking
// at while the answer streams in.
func (s *messageSink) SetKeyboard(keyboard tgbotapi.InlineKeyboardMarkup) {
	if len(s.messageIDs) == 0 {
		return
	}

	edit := tgbotapi.NewEditMessageReplyMarkup(s.chatID, s.messageIDs[0], keyboard)
	if _, err := s.bot.Send(edit); err != nil && !strings.Contains(err.Error(), "message is not modified") {
		log.Printf("Failed to attach keyboard: %v", err)
	}
}

func editMessage(
	bot *tgbotapi.BotAPI,
	chatID int64,
	messageID int,
	text string,
	parseMode string,
) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}

	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	if parseMode != "" {
		editMsg.ParseMode = parseMode
	}

	if _, err := bot.Send(editMsg); err != nil {
		if !strings.Contains(err.Error(), "message is not modified") {
			log.Printf("Telegram edit error (parse_mode=%q): %v", parseMode, err)
		}
		return false
	}

	return true
}

// SplitMessage breaks text into chunks of at most limit runes, preferring
// newline boundaries so that paragraphs and code blocks stay intact.
func SplitMessage(text string, limit int, maxChunks int) []string {
	if limit <= 0 {
		limit = config.ChunkLimit
	}
	if maxChunks <= 0 {
		maxChunks = maxChunksDefault
	}

	text = strings.TrimRight(text, " \t\n")
	if text == "" {
		return nil
	}

	runes := []rune(text)
	if len(runes) <= limit {
		return []string{text}
	}

	var chunks []string

	for start := 0; start < len(runes); {
		if len(chunks) == maxChunks-1 {
			// Last allowed chunk: keep the tail and mark the truncation.
			tail := strings.TrimRight(string(runes[start:]), " \t\n")
			chunks = append(chunks, tail+"\n\n… (truncated)")
			break
		}

		end := start + limit
		if end >= len(runes) {
			chunks = append(chunks, strings.TrimRight(string(runes[start:]), " \t\n"))
			break
		}

		cut := -1
		for i := end; i > start+limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i
				break
			}
		}
		if cut <= 0 {
			cut = end
		}

		chunk := strings.TrimRight(string(runes[start:cut]), " \t\n")
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		start = cut
	}

	if len(chunks) == 0 {
		return nil
	}

	return chunks
}

// -----------------------------------------------------------------------------
// LOADING FEEDBACK
// -----------------------------------------------------------------------------

// runLoadingAnimation keeps editing the placeholder while the model thinks.
// Without it a slow request looks like a frozen chat.
func runLoadingAnimation(
	bot *tgbotapi.BotAPI,
	chatID int64,
	messageID int,
	loadMessage string,
	stop chan bool,
) {
	dots := []string{"", ".", "..", "..."}

	ticker := time.NewTicker(1200 * time.Millisecond)
	defer ticker.Stop()

	for i := 0; ; i = (i + 1) % len(dots) {
		select {
		case <-stop:
			return
		case <-ticker.C:
			editMsg := tgbotapi.NewEditMessageText(chatID, messageID, loadMessage+dots[i])
			if _, err := bot.Send(editMsg); err != nil {
				if !strings.Contains(err.Error(), "message is not modified") {
					log.Printf("Loading edit error: %v", err)
				}
			}
		}
	}
}

func stopLoading(stopAnimation chan bool) {
	select {
	case stopAnimation <- true:
	default:
	}
}

// typingLoop sends the "typing" chat action until the returned stop function
// is called. It is what makes the bot feel alive in clients that render the
// action instead of the placeholder text.
func typingLoop(bot *tgbotapi.BotAPI, chatID int64) func() {
	done := make(chan struct{})
	var once sync.Once

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		send := func() {
			if _, err := bot.Send(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)); err != nil {
				log.Printf("Failed to send chat action: %v", err)
			}
		}

		send()

		ticker := time.NewTicker(typingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				send()
			}
		}
	}()

	return func() {
		once.Do(func() { close(done) })
		wg.Wait()
	}
}
