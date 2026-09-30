// Package groups stores per-chat settings: automatic translation, who is
// allowed to use the bot, and what the bot knows about the chat (title,
// whether it is an administrator there, member roles).
//
// Everything lives in one JSON document so a single write keeps the file
// consistent, and every update is serialised by the write lock.
package groups

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"openrouter-bot/internal/atomicfile"
)

// DefaultLanguage is used when a group has no translation language configured.
const DefaultLanguage = "English"

// maxLanguageLength guards against someone setting a language to a wall of
// text with /translate <anything>.
const maxLanguageLength = 32

// Access modes decide who may trigger the bot inside a chat.
const (
	// AccessEveryone lets every member use the bot. This is the default for
	// a public bot.
	AccessEveryone = "everyone"
	// AccessAdmins restricts the bot to group administrators and the owner.
	AccessAdmins = "admins"
	// AccessOwner restricts the bot to the bot owner (ADMIN_IDS).
	AccessOwner = "owner"
)

// ValidAccess reports whether an access mode is known.
func ValidAccess(mode string) bool {
	switch mode {
	case AccessEveryone, AccessAdmins, AccessOwner:
		return true
	default:
		return false
	}
}

// Member role cache lifetime. Telegram does not push role changes to the bot,
// so a short TTL keeps the answers fresh without a request per message.
const memberCacheTTL = 5 * time.Minute

// Settings is one chat's configuration and metadata.
type Settings struct {
	// TranslateEnabled turns automatic translation of every message on.
	TranslateEnabled bool `json:"enabled"`
	// TargetLanguage is the language messages are translated into.
	TargetLanguage string `json:"target_language"`

	// Access decides who may use the bot in this chat.
	Access string `json:"access"`

	// Title and Members are cached for the admin panel.
	Title   string `json:"title,omitempty"`
	Members int    `json:"members,omitempty"`

	// BotIsAdmin records whether the bot is an administrator of the chat,
	// which is what allows it to read every message in strict privacy mode.
	BotIsAdmin bool `json:"bot_is_admin"`
}

// DefaultSettings returns the settings a new chat starts with.
func DefaultSettings() Settings {
	return Settings{
		TargetLanguage: DefaultLanguage,
		Access:         AccessEveryone,
	}
}

// Normalize fills in missing values, so old files keep working.
func (s Settings) Normalize() Settings {
	if strings.TrimSpace(s.TargetLanguage) == "" {
		s.TargetLanguage = DefaultLanguage
	}
	if !ValidAccess(s.Access) {
		s.Access = AccessEveryone
	}

	return s
}

// TranslationEnabled reports the translation flag.
func (s Settings) TranslationEnabled() bool { return s.TranslateEnabled }

// memberRole is a cached chat-member lookup.
type memberRole struct {
	Role      string
	CheckedAt time.Time
}

// Manager owns the settings for every chat the bot has seen.
type Manager struct {
	mu       sync.RWMutex
	filePath string
	groups   map[int64]Settings
	members  map[int64]map[int64]memberRole
}

// NewManager loads the settings from filePath, falling back to the legacy
// translation file so an upgrade does not lose group configuration.
func NewManager(filePath string) *Manager {
	m := &Manager{
		filePath: filePath,
		groups:   make(map[int64]Settings),
		members:  make(map[int64]map[int64]memberRole),
	}

	if err := m.load(); err != nil {
		log.Printf("Could not load group settings from %s: %v", filePath, err)
	}

	return m
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return m.loadLegacy()
		}
		return err
	}

	var groups map[int64]Settings
	if err := json.Unmarshal(data, &groups); err != nil {
		return err
	}
	if groups == nil {
		groups = make(map[int64]Settings)
	}

	for id, settings := range groups {
		groups[id] = settings.Normalize()
	}

	m.mu.Lock()
	m.groups = groups
	m.mu.Unlock()

	log.Printf("Loaded settings for %d chat(s) from %s", len(groups), m.filePath)

	return nil
}

// loadLegacy reads the translation-only file written by older versions.
func (m *Manager) loadLegacy() error {
	legacyPath := filepath.Join(filepath.Dir(m.filePath), "translations.json")

	data, err := os.ReadFile(legacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var legacy map[int64]struct {
		Enabled        bool   `json:"enabled"`
		TargetLanguage string `json:"target_language"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}

	for id, settings := range legacy {
		m.groups[id] = Settings{
			TranslateEnabled: settings.Enabled,
			TargetLanguage:   settings.TargetLanguage,
			Access:           AccessEveryone,
		}.Normalize()
	}

	log.Printf("Migrated %d chat(s) from %s", len(m.groups), legacyPath)

	return nil
}

// saveLocked writes the settings while the write lock is held.
//
// Serialising the whole marshal-and-write cycle is what makes concurrent
// updates safe: releasing the lock in between lets two writers interleave,
// and the write that finishes last wins even when it started first, silently
// discarding another group's settings.
func (m *Manager) saveLocked() error {
	data, err := json.MarshalIndent(m.groups, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(m.filePath), 0o755); err != nil {
		return err
	}

	return atomicfile.Write(m.filePath, data, 0o644)
}

// Get returns a chat's settings with defaults applied.
func (m *Manager) Get(chatID int64) Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()

	settings, exists := m.groups[chatID]
	if !exists {
		return DefaultSettings()
	}

	return settings.Normalize()
}

// update mutates one chat's settings and persists the result.
func (m *Manager) update(chatID int64, mutate func(*Settings)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	settings := m.groups[chatID].Normalize()
	mutate(&settings)
	m.groups[chatID] = settings

	return m.saveLocked()
}

// SetEnabled turns automatic translation on or off.
func (m *Manager) SetEnabled(chatID int64, enabled bool) error {
	return m.update(chatID, func(settings *Settings) {
		settings.TranslateEnabled = enabled
	})
}

// SetLanguage sets the translation target language.
func (m *Manager) SetLanguage(chatID int64, language string) error {
	language = strings.TrimSpace(language)
	if language == "" {
		return errors.New("language must not be empty")
	}
	if len(language) > maxLanguageLength {
		return errors.New("language name is too long")
	}

	return m.update(chatID, func(settings *Settings) {
		settings.TargetLanguage = language
	})
}

// SetAccess changes who may use the bot in a chat.
func (m *Manager) SetAccess(chatID int64, access string) error {
	if !ValidAccess(access) {
		return errors.New("unknown access mode")
	}

	return m.update(chatID, func(settings *Settings) {
		settings.Access = access
	})
}

// SetTitle remembers the chat title, used by the admin panel.
func (m *Manager) SetTitle(chatID int64, title string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	settings := m.groups[chatID].Normalize()
	settings.Title = strings.TrimSpace(title)
	m.groups[chatID] = settings

	if err := m.saveLocked(); err != nil {
		log.Printf("Could not save title for chat %d: %v", chatID, err)
	}
}

// SetBotAdmin records whether the bot is an administrator of the chat.
func (m *Manager) SetBotAdmin(chatID int64, isAdmin bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	settings := m.groups[chatID].Normalize()
	settings.BotIsAdmin = isAdmin
	m.groups[chatID] = settings

	if err := m.saveLocked(); err != nil {
		log.Printf("Could not save admin flag for chat %d: %v", chatID, err)
	}
}

// BotIsAdmin reports the cached administrator flag.
func (m *Manager) BotIsAdmin(chatID int64) bool {
	return m.Get(chatID).BotIsAdmin
}

// --- member roles ----------------------------------------------------------

// SetMemberRole caches a member lookup so role checks do not call the Telegram
// API on every message.
func (m *Manager) SetMemberRole(chatID, userID int64, role string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.members[chatID] == nil {
		m.members[chatID] = make(map[int64]memberRole)
	}
	m.members[chatID][userID] = memberRole{Role: strings.ToLower(role), CheckedAt: time.Now()}
}

// MemberRole returns the cached role, if it is still fresh.
func (m *Manager) MemberRole(chatID, userID int64) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.members[chatID][userID]
	if !ok {
		return "", false
	}
	if time.Since(entry.CheckedAt) > memberCacheTTL {
		return "", false
	}

	return entry.Role, true
}

// IsAdminRole reports whether a Telegram member status counts as an admin.
func IsAdminRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "creator", "administrator", "owner":
		return true
	default:
		return false
	}
}

// Allowed decides whether a member may use the bot in a chat.
//
// owner is true for the bot's own owners (ADMIN_IDS); they always pass.
func (m *Manager) Allowed(chatID, userID int64, owner bool) bool {
	if owner {
		return true
	}

	switch m.Get(chatID).Access {
	case AccessOwner:
		return false
	case AccessAdmins:
		role, ok := m.MemberRole(chatID, userID)

		return ok && IsAdminRole(role)
	default:
		return true
	}
}

// Chats returns the ids of every chat with stored settings.
func (m *Manager) Chats() []int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids := make([]int64, 0, len(m.groups))
	for id := range m.groups {
		ids = append(ids, id)
	}

	return ids
}

// Count returns how many chats the bot has settings for.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.groups)
}
