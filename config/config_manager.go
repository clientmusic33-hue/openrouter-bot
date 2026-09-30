package config

import (
	"log"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

// Manager owns the active configuration and reloads it when config.yaml
// changes.
//
// Create it once at start-up and share it. Creating additional managers is
// wasteful and unsafe: viper keeps global state, so every extra manager
// registers another file watcher and another reload callback.
type Manager struct {
	configPath string

	mu       sync.RWMutex
	config   *Config
	watching bool

	// modelOverride is the model selected at runtime with /set_model. It is
	// kept separate from the file-backed configuration so that a reload does
	// not silently discard the operator's choice.
	modelOverride string
}

// NewManager loads the configuration from configPath and starts watching it
// for changes. It returns an error instead of exiting so that callers control
// the process lifecycle.
func NewManager(configPath string) (*Manager, error) {
	viper.SetConfigFile(configPath)
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		return nil, err
	}
	log.Printf("Reading configuration from %s", configPath)

	config, err := Load()
	if err != nil {
		return nil, err
	}

	manager := &Manager{
		configPath: configPath,
		config:     config,
	}

	manager.watch()

	return manager, nil
}

// watch installs the config file watcher. viper.WatchConfig must be called
// exactly once per process; calling it repeatedly leaks a goroutine and a
// callback each time.
func (m *Manager) watch() {
	if m.watching {
		return
	}
	m.watching = true

	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Printf("Configuration file changed (%s), reloading", e.Name)
		m.Reload()
	})

	viper.WatchConfig()
}

// Reload re-reads the configuration, preserving any runtime model override.
func (m *Manager) Reload() error {
	updated, err := Load()
	if err != nil {
		log.Printf("Failed to reload configuration, keeping the previous one: %v", err)
		return err
	}

	m.mu.Lock()
	if m.modelOverride != "" {
		updated.Model.ModelName = m.modelOverride
	} else {
		m.modelOverride = updated.Model.ModelName
	}
	m.config = updated
	m.mu.Unlock()

	return nil
}

// GetConfig returns a snapshot copy of the active configuration. Callers get
// their own copy, so they can never race with a reload or with each other.
func (m *Manager) GetConfig() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.config.Clone()
}

// GetModel returns the model currently in use.
func (m *Manager) GetModel() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.config.Model.ModelName
}

// SetModel changes the model for the whole bot and survives config reloads.
func (m *Manager) SetModel(name string) {
	m.mu.Lock()
	m.modelOverride = name
	m.config.Model.ModelName = name
	m.mu.Unlock()
}

// ResetModel restores the model from the configuration file.
func (m *Manager) ResetModel() string {
	m.mu.Lock()
	m.config.Model.ModelName = m.config.Model.ModelNameDefault
	m.modelOverride = m.config.Model.ModelNameDefault
	restored := m.modelOverride
	m.mu.Unlock()

	return restored
}
