package user

import (
	"strings"
	"time"
)

// AddMessage appends a message to the conversation.
func (ut *UsageTracker) AddMessage(role, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}

	ut.History.add(Message{
		Role:      role,
		Content:   content,
		CreatedAt: time.Now(),
	})
}

// GetMessages returns a copy of the conversation.
func (ut *UsageTracker) GetMessages() []Message {
	return ut.History.snapshot()
}

// ClearHistory drops the whole conversation.
func (ut *UsageTracker) ClearHistory() {
	ut.History.clear()
}

// CheckHistory prunes the conversation to at most maxMessages recent messages
// and drops anything older than maxTime minutes.
func (ut *UsageTracker) CheckHistory(maxMessages int, maxTime int) {
	ut.History.prune(maxMessages, time.Duration(maxTime)*time.Minute)
}

// --- History ---------------------------------------------------------------

func NewHistory() *History {
	return &History{messages: make([]Message, 0)}
}

func (h *History) add(message Message) {
	if h == nil {
		return
	}

	h.mu.Lock()
	h.messages = append(h.messages, message)
	h.mu.Unlock()
}

func (h *History) snapshot() []Message {
	if h == nil {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]Message, len(h.messages))
	copy(out, h.messages)

	return out
}

func (h *History) clear() {
	if h == nil {
		return
	}

	h.mu.Lock()
	h.messages = h.messages[:0]
	h.mu.Unlock()
}

// prune removes expired and excess messages. Expiry is judged per message,
// so an idle conversation expires even while other conversations are active.
func (h *History) prune(maxMessages int, maxAge time.Duration) {
	if h == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if maxMessages <= 0 {
		maxMessages = 10
	}

	kept := h.messages
	if maxAge > 0 {
		cutoff := time.Now().Add(-maxAge)
		kept = make([]Message, 0, len(h.messages))
		for _, message := range h.messages {
			if message.CreatedAt.IsZero() || message.CreatedAt.After(cutoff) {
				kept = append(kept, message)
			}
		}
	}

	if len(kept) > maxMessages {
		kept = kept[len(kept)-maxMessages:]
	}

	h.messages = kept
}

// dropLast removes the newest message with the given role. It reports whether
// anything was removed.
func (h *History) dropLast(role string) bool {
	if h == nil {
		return false
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	for i := len(h.messages) - 1; i >= 0; i-- {
		if h.messages[i].Role == role {
			h.messages = append(h.messages[:i], h.messages[i+1:]...)

			return true
		}
	}

	return false
}

// Len returns the number of stored messages.
func (h *History) Len() int {
	if h == nil {
		return 0
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.messages)
}
