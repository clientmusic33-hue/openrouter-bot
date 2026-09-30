// Package grouptranslate stores per-group automatic translation settings.
package grouptranslate

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"sync"

	"openrouter-bot/internal/atomicfile"
)

// DefaultLanguage is used when a group has no language configured.
const DefaultLanguage = "English"

// maxLanguageLength guards against someone setting a language to a wall of
// text with /translate <anything>.
const maxLanguageLength = 32

type Settings struct {
	Enabled        bool   `json:"enabled"`
	TargetLanguage string `json:"target_language"`
}

type Manager struct {
	mu       sync.RWMutex
	filePath string
	groups   map[int64]Settings
}

func NewManager(filePath string) *Manager {
	m := &Manager{
		filePath: filePath,
		groups:   make(map[int64]Settings),
	}

	if err := m.load(); err != nil {
		log.Printf("Could not load translation settings from %s: %v", filePath, err)
	}

	return m
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
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

	m.mu.Lock()
	m.groups = groups
	m.mu.Unlock()

	log.Printf("Loaded translation settings for %d group(s) from %s", len(groups), m.filePath)

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

	return atomicfile.Write(m.filePath, data, 0o644)
}

func (m *Manager) Get(chatID int64) Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()

	settings, exists := m.groups[chatID]
	if !exists {
		return Settings{
			Enabled:        false,
			TargetLanguage: DefaultLanguage,
		}
	}

	if strings.TrimSpace(settings.TargetLanguage) == "" {
		settings.TargetLanguage = DefaultLanguage
	}

	return settings
}

func (m *Manager) SetEnabled(chatID int64, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	settings := m.groups[chatID]
	settings.Enabled = enabled
	if strings.TrimSpace(settings.TargetLanguage) == "" {
		settings.TargetLanguage = DefaultLanguage
	}
	m.groups[chatID] = settings

	return m.saveLocked()
}

func (m *Manager) SetLanguage(chatID int64, language string) error {
	language = strings.TrimSpace(language)
	if language == "" {
		return errors.New("language must not be empty")
	}
	if len(language) > maxLanguageLength {
		return errors.New("language name is too long")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	settings := m.groups[chatID]
	settings.TargetLanguage = language
	m.groups[chatID] = settings

	return m.saveLocked()
}
