// Package memory implements persistent long-term user memory and token-optimised
// short-term conversation summarization.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"openrouter-bot/internal/security"
	"openrouter-bot/storage"
)

const (
	namespaceMemory = "memory"
	maxFactsPerUser = 50
	maxFactLength   = 400
)

// Fact is one long-term memory entry stored for a user.
type Fact struct {
	ID        int       `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// UserMemory holds a user's long-term facts and opt-in/out preference.
type UserMemory struct {
	Enabled bool   `json:"enabled"`
	NextID  int    `json:"next_id"`
	Facts   []Fact `json:"facts"`
}

var sensitiveFactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(password|passwd|secret|api[_ -]?key|private[_ -]?key|bearer|access[_ -]?token|ssn|cvv)\b`),
	regexp.MustCompile(`\b(?:\d[ -]*?){13,19}\b`), // payment card numbers
}

// IsSensitive reports whether text looks like a credential, password, or card
// number that must not be stored in long-term memory.
func IsSensitive(text string) bool {
	if security.RedactSecrets(text) != text {
		return true
	}
	for _, re := range sensitiveFactPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// Manager manages per-user long-term memory backed by storage.Store.
type Manager struct {
	store storage.Store
	mu    sync.RWMutex
	cache map[string]*UserMemory
}

// NewManager creates a long-term memory manager.
func NewManager(store storage.Store) *Manager {
	return &Manager{
		store: store,
		cache: make(map[string]*UserMemory),
	}
}

func (m *Manager) loadLocked(userID string) *UserMemory {
	if mem, ok := m.cache[userID]; ok {
		return mem
	}

	mem := &UserMemory{
		Enabled: true,
		NextID:  1,
		Facts:   []Fact{},
	}

	if m.store != nil {
		if data, err := m.store.Load(context.Background(), namespaceMemory, userID); err == nil && len(data) > 0 {
			_ = json.Unmarshal(data, mem)
			if mem.NextID <= 0 {
				mem.NextID = len(mem.Facts) + 1
			}
		}
	}

	m.cache[userID] = mem
	return mem
}

func (m *Manager) saveLocked(userID string, mem *UserMemory) error {
	if m.store == nil {
		return nil
	}
	data, err := json.MarshalIndent(mem, "", "  ")
	if err != nil {
		return err
	}
	return m.store.Save(context.Background(), namespaceMemory, userID, data)
}

// Enabled reports whether long-term memory is active for userID.
func (m *Manager) Enabled(userID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.loadLocked(userID).Enabled
}

// SetEnabled turns long-term memory on or off for userID.
func (m *Manager) SetEnabled(userID string, enabled bool) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	mem := m.loadLocked(userID)
	mem.Enabled = enabled
	return m.saveLocked(userID, mem)
}

// Add stores a new long-term fact for userID after checking for sensitivity and
// duplicates.
func (m *Manager) Add(userID, text string) (Fact, error) {
	if m == nil {
		return Fact{}, errors.New("memory manager not initialised")
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return Fact{}, errors.New("memory fact must not be empty")
	}
	if IsSensitive(text) {
		return Fact{}, errors.New("refusing to store sensitive information (passwords, keys, or tokens)")
	}
	if len([]rune(text)) > maxFactLength {
		runes := []rune(text)
		text = string(runes[:maxFactLength])
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	mem := m.loadLocked(userID)
	if !mem.Enabled {
		return Fact{}, errors.New("memory is disabled; enable it with /memory on")
	}

	// Deduplicate existing identical facts.
	for _, existing := range mem.Facts {
		if strings.EqualFold(existing.Text, text) {
			return existing, nil
		}
	}

	if mem.NextID <= 0 {
		mem.NextID = len(mem.Facts) + 1
	}
	fact := Fact{
		ID:        mem.NextID,
		Text:      text,
		CreatedAt: time.Now(),
	}
	mem.NextID++
	mem.Facts = append(mem.Facts, fact)

	if len(mem.Facts) > maxFactsPerUser {
		mem.Facts = mem.Facts[len(mem.Facts)-maxFactsPerUser:]
	}

	if err := m.saveLocked(userID, mem); err != nil {
		return Fact{}, err
	}
	return fact, nil
}

// MaybeExtractImplicitFact checks if a user message explicitly states a stable
// personal preference (e.g. "Remember that I use Go 1.24" or "My preferred
// language is Python") and saves it when memory is enabled.
func (m *Manager) MaybeExtractImplicitFact(userID, message string) (Fact, bool) {
	if m == nil || !m.Enabled(userID) {
		return Fact{}, false
	}

	trimmed := strings.TrimSpace(message)
	lower := strings.ToLower(trimmed)

	prefixes := []string{
		"remember that ",
		"remember: ",
		"please remember ",
		"note that i ",
		"i always prefer ",
		"my preferred ",
	}

	for _, p := range prefixes {
		if idx := strings.Index(lower, p); idx == 0 {
			factText := strings.TrimSpace(trimmed[len(p):])
			if len(factText) >= 4 && len(factText) <= 250 && !IsSensitive(factText) {
				if fact, err := m.Add(userID, factText); err == nil {
					return fact, true
				}
			}
		}
	}

	return Fact{}, false
}

// List returns a copy of all stored facts for userID.
func (m *Manager) List(userID string) []Fact {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	mem := m.loadLocked(userID)
	out := make([]Fact, len(mem.Facts))
	copy(out, mem.Facts)
	return out
}

// Forget removes a fact by numeric ID or by substring match.
func (m *Manager) Forget(userID, target string) (int, error) {
	if m == nil {
		return 0, nil
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return 0, errors.New("specify a memory ID or keyword to forget")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	mem := m.loadLocked(userID)
	id, idErr := strconv.Atoi(target)
	lower := strings.ToLower(target)

	kept := make([]Fact, 0, len(mem.Facts))
	removed := 0
	for _, f := range mem.Facts {
		if (idErr == nil && f.ID == id) || strings.Contains(strings.ToLower(f.Text), lower) {
			removed++
			continue
		}
		kept = append(kept, f)
	}

	mem.Facts = kept
	return removed, m.saveLocked(userID, mem)
}

// Clear removes all stored facts for userID.
func (m *Manager) Clear(userID string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	mem := m.loadLocked(userID)
	mem.Facts = nil
	mem.NextID = 1
	return m.saveLocked(userID, mem)
}

// RelevantContext formats the most relevant long-term facts for prompt
// injection, scoring by keyword overlap with the current query and capping
// output length.
func (m *Manager) RelevantContext(userID, query string, maxFacts int) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	mem := m.loadLocked(userID)
	if !mem.Enabled || len(mem.Facts) == 0 {
		m.mu.Unlock()
		return ""
	}
	facts := make([]Fact, len(mem.Facts))
	copy(facts, mem.Facts)
	m.mu.Unlock()

	if maxFacts <= 0 {
		maxFacts = 8
	}

	queryWords := strings.Fields(strings.ToLower(query))

	// Select facts that overlap with query words first, then most recent facts.
	var matched, recent []Fact
	for i := len(facts) - 1; i >= 0; i-- {
		f := facts[i]
		fLower := strings.ToLower(f.Text)
		hit := false
		for _, w := range queryWords {
			if len(w) >= 3 && strings.Contains(fLower, w) {
				hit = true
				break
			}
		}
		if hit {
			matched = append(matched, f)
		} else {
			recent = append(recent, f)
		}
	}

	combined := append(matched, recent...)
	if len(combined) > maxFacts {
		combined = combined[:maxFacts]
	}

	var sb strings.Builder
	sb.WriteString("Long-term user memory:\n")
	for _, f := range combined {
		sb.WriteString(fmt.Sprintf("- %s\n", f.Text))
	}
	return strings.TrimSpace(sb.String())
}
