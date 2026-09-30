package grouptranslate

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDefaultsForUnknownGroup(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "translations.json"))

	settings := m.Get(123)

	if settings.Enabled {
		t.Error("an unknown group should not have translation enabled")
	}
	if settings.TargetLanguage != DefaultLanguage {
		t.Errorf("TargetLanguage = %q, want %q", settings.TargetLanguage, DefaultLanguage)
	}
}

func TestSetAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "translations.json")

	m := NewManager(path)

	if err := m.SetLanguage(42, "Hindi"); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	if err := m.SetEnabled(42, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	// A fresh manager must see the persisted settings.
	reloaded := NewManager(path)
	settings := reloaded.Get(42)

	if !settings.Enabled {
		t.Error("enabled state was not persisted")
	}
	if settings.TargetLanguage != "Hindi" {
		t.Errorf("TargetLanguage = %q, want Hindi", settings.TargetLanguage)
	}
}

func TestSetLanguageValidation(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "translations.json"))

	if err := m.SetLanguage(1, "   "); err == nil {
		t.Error("an empty language should be rejected")
	}
	if err := m.SetLanguage(1, "Hindi"); err != nil {
		t.Fatalf("a short language should be accepted: %v", err)
	}
	if err := m.SetLanguage(1, longString(100)); err == nil {
		t.Error("an over-long language should be rejected")
	}
}

// TestSaveIsAtomic ensures a crash cannot leave a truncated file behind: the
// settings file must always be valid JSON.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "translations.json")

	m := NewManager(path)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = m.SetEnabled(int64(i), true)
		}(i)
	}
	wg.Wait()

	// A second manager can parse whatever is on disk.
	reloaded := NewManager(path)
	for i := 0; i < 20; i++ {
		if !reloaded.Get(int64(i)).Enabled {
			t.Errorf("group %d lost its setting", i)
		}
	}
}

func TestNoLeftoverTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "translations.json")

	m := NewManager(path)
	if err := m.SetEnabled(1, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if len(entry.Name()) > 4 && entry.Name()[:4] == ".tmp" {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func longString(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'a'
	}

	return string(out)
}
