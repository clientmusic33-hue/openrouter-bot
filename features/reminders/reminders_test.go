package reminders

import (
	"testing"
	"time"

	jsonstore "openrouter-bot/storage/json"
)

func TestRemindersSurviveRestartAndPopDue(t *testing.T) {
	store := jsonstore.New(t.TempDir())
	mgr := NewManager(store)

	now := time.Now()
	dueAt, text, err := ParseRemindCommand(now, "10m Deploy release")
	if err != nil {
		t.Fatalf("ParseRemindCommand: %v", err)
	}
	if text != "Deploy release" || dueAt.Before(now.Add(9*time.Minute)) {
		t.Fatalf("unexpected parse result: dueAt=%v text=%q", dueAt, text)
	}

	if _, err := mgr.AddReminder("42", 1001, now.Add(-time.Minute), "Overdue task"); err != nil {
		t.Fatalf("AddReminder: %v", err)
	}
	if _, err := mgr.AddReminder("42", 1001, now.Add(time.Hour), "Future task"); err != nil {
		t.Fatalf("AddReminder: %v", err)
	}

	// Simulate bot restart by creating a new Manager on the same store.
	reloaded := NewManager(store)
	if len(reloaded.ListReminders("42")) != 2 {
		t.Fatalf("expected 2 persisted reminders after restart, got %d", len(reloaded.ListReminders("42")))
	}

	due := reloaded.PopDue(now)
	if len(due) != 1 || due[0].Text != "Overdue task" {
		t.Fatalf("PopDue = %+v, want [Overdue task]", due)
	}

	if len(reloaded.ListReminders("42")) != 1 {
		t.Fatalf("expected 1 remaining reminder, got %d", len(reloaded.ListReminders("42")))
	}
}

func TestNotesAndTasksPersistence(t *testing.T) {
	mgr := NewManager(jsonstore.New(t.TempDir()))

	n, err := mgr.AddNote("u1", "Server IP is in vault")
	if err != nil || n.ID != 1 {
		t.Fatalf("AddNote: %v, %+v", err, n)
	}
	if len(mgr.ListNotes("u1")) != 1 {
		t.Fatal("expected 1 note")
	}

	task, err := mgr.AddTask("u1", "Write unit tests")
	if err != nil || task.ID != 1 {
		t.Fatalf("AddTask: %v, %+v", err, task)
	}
	ok, err := mgr.CompleteTask("u1", task.ID)
	if err != nil || !ok {
		t.Fatalf("CompleteTask: %v, %v", ok, err)
	}
	tasks := mgr.ListTasks("u1")
	if len(tasks) != 1 || !tasks[0].Done {
		t.Fatalf("expected completed task, got %+v", tasks)
	}
}
