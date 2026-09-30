// Package user tracks per-user spend, conversation state and rate limits.
package user

import (
	"strconv"
	"sync"

	"openrouter-bot/config"
)

// historyKey identifies a conversation. History is scoped to the pair
// (chat, user) so that a private conversation and each group a user is in
// stay separate, while spend stays attached to the user.
type historyKey struct {
	ChatID int64
	UserID int64
}

type Manager struct {
	LogsDir string

	mu        sync.Mutex
	users     map[int64]*UsageTracker
	histories map[historyKey]*History
}

func NewUserManager(logsDir string) *Manager {
	return &Manager{
		LogsDir:   logsDir,
		users:     make(map[int64]*UsageTracker),
		histories: make(map[historyKey]*History),
	}
}

// GetUser returns the tracker for a user in a chat, creating both the tracker
// and the conversation on first use.
func (um *Manager) GetUser(chatID, userID int64, userName string, conf *config.Config) *UsageTracker {
	um.mu.Lock()
	defer um.mu.Unlock()

	tracker, exists := um.users[userID]
	if !exists {
		tracker = NewUsageTracker(
			strconv.FormatInt(userID, 10),
			userName,
			um.LogsDir,
			conf,
			nil,
		)
		um.users[userID] = tracker
	}

	// Keep the stored user name fresh.
	if userName != "" {
		tracker.UserName = userName
	}

	key := historyKey{ChatID: chatID, UserID: userID}
	history, exists := um.histories[key]
	if !exists {
		history = NewHistory()
		um.histories[key] = history
	}
	tracker.History = history

	return tracker
}

// ClearHistory drops the conversation for a single chat.
func (um *Manager) ClearHistory(chatID, userID int64) {
	um.mu.Lock()
	defer um.mu.Unlock()

	if history, exists := um.histories[historyKey{ChatID: chatID, UserID: userID}]; exists {
		history.clear()
	}
}

// ActiveUsers returns the number of users seen so far.
func (um *Manager) ActiveUsers() int {
	um.mu.Lock()
	defer um.mu.Unlock()

	return len(um.users)
}
