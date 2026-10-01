// Package reminders manages persistent user reminders, tasks, and notes.
package reminders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"openrouter-bot/storage"
)

const (
	nsReminders = "reminders"
	nsNotes     = "notes"
	nsTasks     = "tasks"

	maxItemsPerUser = 100
	maxTextLen      = 1000
)

// Reminder is a scheduled notification that survives bot restarts.
type Reminder struct {
	ID        int       `json:"id"`
	UserID    string    `json:"user_id"`
	ChatID    int64     `json:"chat_id"`
	Text      string    `json:"text"`
	DueAt     time.Time `json:"due_at"`
	CreatedAt time.Time `json:"created_at"`
	Delivered bool      `json:"delivered"`
}

// Note is a persistent user note.
type Note struct {
	ID        int       `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// Task is a persistent user todo item.
type Task struct {
	ID        int       `json:"id"`
	Text      string    `json:"text"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"created_at"`
}

// Manager owns reminders, notes, and tasks persisted in a storage.Store.
type Manager struct {
	store     storage.Store
	mu        sync.Mutex
	reminders []Reminder
	nextRemID int
}

// NewManager loads existing reminders from store.
func NewManager(store storage.Store) *Manager {
	m := &Manager{
		store:     store,
		nextRemID: 1,
	}
	m.loadReminders()
	return m
}

func (m *Manager) loadReminders() {
	if m.store == nil {
		return
	}
	data, err := m.store.Load(context.Background(), nsReminders, "all")
	if err != nil || len(data) == 0 {
		return
	}
	var list []Reminder
	if err := json.Unmarshal(data, &list); err != nil {
		return
	}
	maxID := 0
	for _, r := range list {
		if r.ID > maxID {
			maxID = r.ID
		}
	}
	m.reminders = list
	m.nextRemID = maxID + 1
}

func (m *Manager) saveRemindersLocked() error {
	if m.store == nil {
		return nil
	}
	data, err := json.MarshalIndent(m.reminders, "", "  ")
	if err != nil {
		return err
	}
	return m.store.Save(context.Background(), nsReminders, "all", data)
}

var durationSpecRe = regexp.MustCompile(`(?i)^(\d+)\s*(s|sec|secs|second|seconds|m|min|mins|minute|minutes|h|hr|hrs|hour|hours|d|day|days|w|week|weeks)\b\s*(.*)$`)

// ParseRemindCommand parses "/remind <when> <text>", supporting relative
// durations ("10m", "2h", "3d", "in 15m") as well as Go durations ("1h30m")
// and ISO timestamps ("2026-10-02 14:30").
func ParseRemindCommand(now time.Time, raw string) (time.Time, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, "", errors.New("usage: /remind <time> <message> (e.g. /remind 30m Check build)")
	}

	if strings.HasPrefix(strings.ToLower(raw), "in ") {
		raw = strings.TrimSpace(raw[3:])
	}

	if matches := durationSpecRe.FindStringSubmatch(raw); len(matches) == 4 {
		n, _ := strconv.Atoi(matches[1])
		unit := strings.ToLower(matches[2])
		text := strings.TrimSpace(matches[3])
		if text == "" {
			return time.Time{}, "", errors.New("reminder text must not be empty")
		}
		if n <= 0 {
			return time.Time{}, "", errors.New("duration must be positive")
		}

		var d time.Duration
		switch unit[0] {
		case 's':
			d = time.Duration(n) * time.Second
		case 'm':
			d = time.Duration(n) * time.Minute
		case 'h':
			d = time.Duration(n) * time.Hour
		case 'd':
			d = time.Duration(n) * 24 * time.Hour
		case 'w':
			d = time.Duration(n) * 7 * 24 * time.Hour
		}
		return now.Add(d), clampText(text), nil
	}

	parts := strings.Fields(raw)
	if len(parts) >= 2 {
		if d, err := time.ParseDuration(parts[0]); err == nil && d > 0 {
			text := strings.TrimSpace(strings.TrimPrefix(raw, parts[0]))
			if text == "" {
				return time.Time{}, "", errors.New("reminder text must not be empty")
			}
			return now.Add(d), clampText(text), nil
		}
	}

	if len(parts) >= 3 {
		candidateStamp := parts[0] + " " + parts[1]
		if t, err := time.Parse("2006-01-02 15:04", candidateStamp); err == nil {
			text := strings.TrimSpace(strings.Join(parts[2:], " "))
			if text == "" {
				return time.Time{}, "", errors.New("reminder text must not be empty")
			}
			return t, clampText(text), nil
		}
	}

	return time.Time{}, "", errors.New("could not parse time; try e.g. /remind 15m Call Alex or /remind 2h Review PR")
}

// AddReminder schedules a persistent reminder.
func (m *Manager) AddReminder(userID string, chatID int64, dueAt time.Time, text string) (Reminder, error) {
	text = clampText(strings.TrimSpace(text))
	if text == "" {
		return Reminder{}, errors.New("reminder text must not be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r := Reminder{
		ID:        m.nextRemID,
		UserID:    userID,
		ChatID:    chatID,
		Text:      text,
		DueAt:     dueAt,
		CreatedAt: time.Now(),
	}
	m.nextRemID++
	m.reminders = append(m.reminders, r)

	return r, m.saveRemindersLocked()
}

// ListReminders returns pending reminders for userID ordered by DueAt.
func (m *Manager) ListReminders(userID string) []Reminder {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Reminder
	for _, r := range m.reminders {
		if r.UserID == userID && !r.Delivered {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].DueAt.Before(out[j].DueAt)
	})
	return out
}

// DeleteReminder cancels a pending reminder for userID.
func (m *Manager) DeleteReminder(userID string, id int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	kept := make([]Reminder, 0, len(m.reminders))
	removed := false
	for _, r := range m.reminders {
		if r.UserID == userID && r.ID == id {
			removed = true
			continue
		}
		kept = append(kept, r)
	}
	m.reminders = kept
	return removed, m.saveRemindersLocked()
}

// PopDue returns and marks delivered all reminders whose DueAt <= now.
func (m *Manager) PopDue(now time.Time) []Reminder {
	m.mu.Lock()
	defer m.mu.Unlock()

	var due []Reminder
	changed := false
	kept := make([]Reminder, 0, len(m.reminders))

	for _, r := range m.reminders {
		if !r.Delivered && !r.DueAt.After(now) {
			r.Delivered = true
			due = append(due, r)
			changed = true
			continue
		}
		if !r.Delivered {
			kept = append(kept, r)
		}
	}

	if changed {
		m.reminders = kept
		_ = m.saveRemindersLocked()
	}

	return due
}

// --- Notes -----------------------------------------------------------------

func (m *Manager) loadNotesLocked(userID string) []Note {
	if m.store == nil {
		return nil
	}
	data, err := m.store.Load(context.Background(), nsNotes, userID)
	if err != nil || len(data) == 0 {
		return nil
	}
	var notes []Note
	_ = json.Unmarshal(data, &notes)
	return notes
}

func (m *Manager) saveNotesLocked(userID string, notes []Note) error {
	if m.store == nil {
		return nil
	}
	data, err := json.MarshalIndent(notes, "", "  ")
	if err != nil {
		return err
	}
	return m.store.Save(context.Background(), nsNotes, userID, data)
}

// AddNote saves a note for userID.
func (m *Manager) AddNote(userID, text string) (Note, error) {
	text = clampText(strings.TrimSpace(text))
	if text == "" {
		return Note{}, errors.New("note text must not be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	notes := m.loadNotesLocked(userID)
	nextID := 1
	for _, n := range notes {
		if n.ID >= nextID {
			nextID = n.ID + 1
		}
	}

	note := Note{
		ID:        nextID,
		Text:      text,
		CreatedAt: time.Now(),
	}
	notes = append(notes, note)
	if len(notes) > maxItemsPerUser {
		notes = notes[len(notes)-maxItemsPerUser:]
	}

	return note, m.saveNotesLocked(userID, notes)
}

// ListNotes returns all notes for userID.
func (m *Manager) ListNotes(userID string) []Note {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.loadNotesLocked(userID)
}

// DeleteNote deletes a note by ID for userID.
func (m *Manager) DeleteNote(userID string, id int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	notes := m.loadNotesLocked(userID)
	kept := make([]Note, 0, len(notes))
	removed := false
	for _, n := range notes {
		if n.ID == id {
			removed = true
			continue
		}
		kept = append(kept, n)
	}
	return removed, m.saveNotesLocked(userID, kept)
}

// ClearNotes deletes all notes for userID.
func (m *Manager) ClearNotes(userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.saveNotesLocked(userID, nil)
}

// --- Tasks -----------------------------------------------------------------

func (m *Manager) loadTasksLocked(userID string) []Task {
	if m.store == nil {
		return nil
	}
	data, err := m.store.Load(context.Background(), nsTasks, userID)
	if err != nil || len(data) == 0 {
		return nil
	}
	var tasks []Task
	_ = json.Unmarshal(data, &tasks)
	return tasks
}

func (m *Manager) saveTasksLocked(userID string, tasks []Task) error {
	if m.store == nil {
		return nil
	}
	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	return m.store.Save(context.Background(), nsTasks, userID, data)
}

// AddTask adds a new task for userID.
func (m *Manager) AddTask(userID, text string) (Task, error) {
	text = clampText(strings.TrimSpace(text))
	if text == "" {
		return Task{}, errors.New("task description must not be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	tasks := m.loadTasksLocked(userID)
	nextID := 1
	for _, t := range tasks {
		if t.ID >= nextID {
			nextID = t.ID + 1
		}
	}

	task := Task{
		ID:        nextID,
		Text:      text,
		CreatedAt: time.Now(),
	}
	tasks = append(tasks, task)
	if len(tasks) > maxItemsPerUser {
		tasks = tasks[len(tasks)-maxItemsPerUser:]
	}

	return task, m.saveTasksLocked(userID, tasks)
}

// ListTasks returns all tasks for userID.
func (m *Manager) ListTasks(userID string) []Task {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.loadTasksLocked(userID)
}

// CompleteTask marks a task as done by ID.
func (m *Manager) CompleteTask(userID string, id int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tasks := m.loadTasksLocked(userID)
	found := false
	for i := range tasks {
		if tasks[i].ID == id {
			tasks[i].Done = true
			found = true
			break
		}
	}
	if !found {
		return false, nil
	}
	return true, m.saveTasksLocked(userID, tasks)
}

// DeleteTask removes a task by ID.
func (m *Manager) DeleteTask(userID string, id int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tasks := m.loadTasksLocked(userID)
	kept := make([]Task, 0, len(tasks))
	removed := false
	for _, t := range tasks {
		if t.ID == id {
			removed = true
			continue
		}
		kept = append(kept, t)
	}
	return removed, m.saveTasksLocked(userID, kept)
}

func clampText(s string) string {
	runes := []rune(s)
	if len(runes) > maxTextLen {
		return fmt.Sprintf("%s…", string(runes[:maxTextLen]))
	}
	return s
}
