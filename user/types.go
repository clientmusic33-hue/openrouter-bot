package user

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sashabaranov/go-openai"
)

// Roles assigned to a Telegram user.
const (
	RoleAdmin = "ADMIN"
	RoleUser  = "USER"
	RoleGuest = "GUEST"
)

// UsageTracker stores everything the bot knows about one Telegram user:
// spend, conversation history, system prompt and the currently running
// request.
//
// All mutable state is guarded by a mutex. The tracker is shared between the
// update loop goroutine (commands) and one or more request goroutines, so
// unsynchronised access here is a data race.
type UsageTracker struct {
	UserID   string
	UserName string
	LogsDir  string

	// mu guards the scalar fields below.
	mu              sync.Mutex
	systemPrompt    string
	lastMessageTime time.Time

	// Rate limiting window.
	rateWindowStart time.Time
	rateCount       int

	// ChatMu serialises AI requests for this user so that a second message
	// cannot start a second generation while the first is streaming.
	ChatMu sync.Mutex

	// streamMu guards the active stream and its cancel function.
	streamMu     sync.Mutex
	stream       *openai.ChatCompletionStream
	streamCancel context.CancelFunc

	Usage   *UserUsage
	History *History

	UsageMu sync.Mutex
	FileMu  sync.Mutex
}

type Message struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// History is a bounded conversation log. It is shared by pointer so that a
// user can hold a different history per chat while keeping a single spend
// record.
type History struct {
	mu       sync.Mutex
	messages []Message
}

type UserUsage struct {
	UserName     string    `json:"user_name"`
	UsageHistory UsageHist `json:"usage_history"`

	// Running statistics used by /recommend. Kept as running values so no
	// history of message contents has to be stored.
	AvgPromptChars float64 `json:"avg_prompt_chars"`
	PromptSamples  int64   `json:"prompt_samples"`
	UsedVision     bool    `json:"used_vision"`

	// Settings holds the user's own model choice and UI preferences.
	Settings Settings `json:"settings"`

	// LastModel is the model that produced the newest answer. Feedback and the
	// footer need to name it without looking at the whole conversation.
	LastModel string `json:"last_model,omitempty"`
}

type Cost struct {
	Day        float64 `json:"day"`
	Month      float64 `json:"month"`
	AllTime    float64 `json:"all_time"`
	LastUpdate string  `json:"last_update"`
}

type UsageHist struct {
	ChatCost map[string]float64 `json:"chat_cost"`
	Requests map[string]int     `json:"requests"`
}

type GenerationResponse struct {
	Data GenerationData `json:"data"`
}

type GenerationData struct {
	ID                     string  `json:"id"`
	Model                  string  `json:"model"`
	Streamed               bool    `json:"streamed"`
	GenerationTime         int     `json:"generation_time"`
	CreatedAt              string  `json:"created_at"`
	TokensPrompt           int     `json:"tokens_prompt"`
	TokensCompletion       int     `json:"tokens_completion"`
	NativeTokensPrompt     int     `json:"native_tokens_prompt"`
	NativeTokensCompletion int     `json:"native_tokens_completion"`
	NumMediaPrompt         int     `json:"num_media_prompt"`
	NumMediaCompletion     int     `json:"num_media_completion"`
	Origin                 string  `json:"origin"`
	TotalCost              float64 `json:"total_cost"`
}

// --- system prompt ---------------------------------------------------------

func (ut *UsageTracker) GetSystemPrompt() string {
	ut.mu.Lock()
	defer ut.mu.Unlock()

	return ut.systemPrompt
}

func (ut *UsageTracker) SetSystemPrompt(prompt string) {
	ut.mu.Lock()
	ut.systemPrompt = prompt
	ut.mu.Unlock()
}

// --- activity tracking -----------------------------------------------------

// Touch records that the user was just active.
func (ut *UsageTracker) Touch() {
	ut.mu.Lock()
	ut.lastMessageTime = time.Now()
	ut.mu.Unlock()
}

// LastMessage returns the last time the user was active.
func (ut *UsageTracker) LastMessage() time.Time {
	ut.mu.Lock()
	defer ut.mu.Unlock()

	return ut.lastMessageTime
}

// --- active stream ---------------------------------------------------------

// SetStream registers the running generation and the function that aborts it.
func (ut *UsageTracker) SetStream(stream *openai.ChatCompletionStream, cancel context.CancelFunc) {
	ut.streamMu.Lock()
	ut.stream = stream
	ut.streamCancel = cancel
	ut.streamMu.Unlock()
}

// ClearStream drops the reference to the finished generation.
func (ut *UsageTracker) ClearStream() {
	ut.streamMu.Lock()
	ut.stream = nil
	ut.streamCancel = nil
	ut.streamMu.Unlock()
}

// HasStream reports whether a generation is currently running.
func (ut *UsageTracker) HasStream() bool {
	ut.streamMu.Lock()
	defer ut.streamMu.Unlock()

	return ut.stream != nil
}

// StopStream aborts the running generation, if any. It returns true when a
// generation was actually stopped.
//
// Cancelling the request context is what makes the blocked Recv() call
// return; closing the stream alone is not enough to unblock it.
func (ut *UsageTracker) StopStream() bool {
	ut.streamMu.Lock()
	stream := ut.stream
	cancel := ut.streamCancel
	ut.stream = nil
	ut.streamCancel = nil
	ut.streamMu.Unlock()

	if cancel != nil {
		cancel()
	}

	if stream != nil {
		_ = stream.Close()
		return true
	}

	return cancel != nil
}

// -----------------------------------------------------------------------------
// CONVERSATION CONTROL
// -----------------------------------------------------------------------------

// LastUserMessage returns the most recent user prompt in this conversation.
// Used by "regenerate", which repeats the last question.
func (ut *UsageTracker) LastUserMessage() (string, bool) {
	messages := ut.GetMessages()

	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
			return messages[i].Content, true
		}
	}

	return "", false
}

// DropLastAssistant removes the final assistant message, so a regenerated
// answer does not stack on top of the one it replaces. It returns the role of
// the message it removed.
func (ut *UsageTracker) DropLastAssistant() bool {
	return ut.History.dropLast("assistant")
}

// DropLastUser removes the final user message. Regenerating replays the prompt
// itself, so the copy kept in history would otherwise be duplicated.
func (ut *UsageTracker) DropLastUser() bool {
	return ut.History.dropLast("user")
}

// LastModel returns the model that produced the newest answer, if any.
func (ut *UsageTracker) LastModel() string {
	ut.UsageMu.Lock()
	defer ut.UsageMu.Unlock()

	if ut.Usage == nil {
		return ""
	}

	return ut.Usage.LastModel
}

// SetLastModel remembers which model answered, so feedback and the footer can
// name it.
func (ut *UsageTracker) SetLastModel(model string) {
	ut.updateUsage(func(usage *UserUsage) {
		usage.LastModel = strings.TrimSpace(model)
	})
}

// RequestCount returns how many requests the user has made in total.
func (ut *UsageTracker) RequestCount() int64 {
	ut.UsageMu.Lock()
	defer ut.UsageMu.Unlock()

	var total int64
	if ut.Usage == nil {
		return 0
	}
	for _, count := range ut.Usage.UsageHistory.Requests {
		total += int64(count)
	}

	return total
}
