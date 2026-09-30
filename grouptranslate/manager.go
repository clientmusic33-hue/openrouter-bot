package grouptranslate

import (
	"encoding/json"
	"os"
	"sync"
)

type Settings struct {
	Enabled       bool   `json:"enabled"`
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

	_ = m.load()

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

	m.mu.Lock()
	defer m.mu.Unlock()

	return json.Unmarshal(data, &m.groups)
}

func (m *Manager) save() error {
	m.mu.RLock()
	data, err := json.MarshalIndent(m.groups, "", "  ")
	m.mu.RUnlock()

	if err != nil {
		return err
	}

	if err := os.MkdirAll("data", 0755); err != nil {
		return err
	}

	return os.WriteFile(m.filePath, data, 0644)
}

func (m *Manager) Get(chatID int64) Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()

	settings, exists := m.groups[chatID]

	if !exists {
		return Settings{
			Enabled:        false,
			TargetLanguage: "English",
		}
	}

	return settings
}

func (m *Manager) SetEnabled(chatID int64, enabled bool) error {
	m.mu.Lock()

	settings := m.groups[chatID]
	settings.Enabled = enabled

	if settings.TargetLanguage == "" {
		settings.TargetLanguage = "English"
	}

	m.groups[chatID] = settings
	m.mu.Unlock()

	return m.save()
}

func (m *Manager) SetLanguage(chatID int64, language string) error {
	m.mu.Lock()

	settings := m.groups[chatID]
	settings.TargetLanguage = language

	if settings.TargetLanguage == "" {
		settings.TargetLanguage = "English"
	}

	m.groups[chatID] = settings
	m.mu.Unlock()

	return m.save()
}
