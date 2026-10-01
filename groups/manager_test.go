package groups

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDefaultsForUnknownGroup(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "groups.json"))

	settings := m.Get(123)

	if settings.TranslateEnabled {
		t.Error("an unknown group should not have translation enabled")
	}
	if settings.TargetLanguage != DefaultLanguage {
		t.Errorf("TargetLanguage = %q, want %q", settings.TargetLanguage, DefaultLanguage)
	}
	if settings.Access != AccessEveryone {
		t.Errorf("Access = %q, want %q", settings.Access, AccessEveryone)
	}
}

func TestSetAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.json")

	m := NewManager(path)

	if err := m.SetLanguage(42, "Hindi"); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	if err := m.SetEnabled(42, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := m.SetAccess(42, AccessAdmins); err != nil {
		t.Fatalf("SetAccess: %v", err)
	}

	// A fresh manager must see the persisted settings.
	reloaded := NewManager(path)
	settings := reloaded.Get(42)

	if !settings.TranslateEnabled {
		t.Error("enabled state was not persisted")
	}
	if settings.TargetLanguage != "Hindi" {
		t.Errorf("TargetLanguage = %q, want Hindi", settings.TargetLanguage)
	}
	if settings.Access != AccessAdmins {
		t.Errorf("Access = %q, want %q", settings.Access, AccessAdmins)
	}
}

func TestSetLanguageValidation(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "groups.json"))

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

func TestSetAccessValidation(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "groups.json"))

	if err := m.SetAccess(1, "nobody"); err == nil {
		t.Error("an unknown access mode should be rejected")
	}
	if err := m.SetAccess(1, AccessOwner); err != nil {
		t.Fatalf("a known access mode should be accepted: %v", err)
	}
}

// TestLegacyMigration keeps group translation working after the upgrade from
// the translation-only settings file.
func TestLegacyMigration(t *testing.T) {
	dir := t.TempDir()

	legacy := map[string]struct {
		Enabled        bool   `json:"enabled"`
		TargetLanguage string `json:"target_language"`
	}{
		"99": {Enabled: true, TargetLanguage: "Russian"},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "translations.json"), data, 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	m := NewManager(filepath.Join(dir, "groups.json"))
	settings := m.Get(99)

	if !settings.TranslateEnabled || settings.TargetLanguage != "Russian" {
		t.Errorf("legacy settings were not migrated: %#v", settings)
	}
}

func TestAccessModes(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "groups.json"))

	// everyone: any member may use the bot.
	if !m.Allowed(-100, 7, false) {
		t.Error("everyone mode should allow a plain member")
	}

	// admins: only a cached admin role passes.
	if err := m.SetAccess(-100, AccessAdmins); err != nil {
		t.Fatalf("SetAccess: %v", err)
	}
	if m.Allowed(-100, 7, false) {
		t.Error("admins mode should reject an unknown member")
	}
	m.SetMemberRole(-100, 7, "administrator")
	if !m.Allowed(-100, 7, false) {
		t.Error("admins mode should allow an administrator")
	}
	m.SetMemberRole(-100, 8, "member")
	if m.Allowed(-100, 8, false) {
		t.Error("admins mode should reject a plain member")
	}

	// owner: only the bot owner passes, regardless of the group role.
	if err := m.SetAccess(-100, AccessOwner); err != nil {
		t.Fatalf("SetAccess: %v", err)
	}
	if m.Allowed(-100, 7, false) {
		t.Error("owner mode should reject a group administrator")
	}
	if !m.Allowed(-100, 7, true) {
		t.Error("owner mode should allow the bot owner")
	}
}

// TestSaveIsAtomic ensures a crash cannot leave a truncated file behind: the
// settings file must always be valid JSON.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")

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
		if !reloaded.Get(int64(i)).TranslateEnabled {
			t.Errorf("group %d lost its setting", i)
		}
	}
}

func TestNoLeftoverTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")

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
