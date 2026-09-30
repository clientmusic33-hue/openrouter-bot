package lang

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func writeLanguages(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	files := map[string]string{
		"EN.json": `{"language":"english","commands":{"reset":"Memory cleared."},"only_en":"english only"}`,
		"RU.json": `{"language":"russian","commands":{"reset":"Память очищена."}}`,
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	return dir
}

func TestLoadTranslations(t *testing.T) {
	dir := writeLanguages(t)

	if err := LoadTranslations(dir); err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}

	got := Languages()
	if len(got) != 2 || got[0] != "EN" || got[1] != "RU" {
		t.Fatalf("Languages() = %v, want [EN RU]", got)
	}
}

// TestTranslateIsCaseInsensitive is a regression test: the configuration
// default used to be lower case "en" while the bundles are keyed "EN", so
// every string degraded to its raw key.
func TestTranslateIsCaseInsensitive(t *testing.T) {
	if err := LoadTranslations(writeLanguages(t)); err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}

	cases := []string{"EN", "en", " En ", "EN "}

	for _, language := range cases {
		if got := Translate("commands.reset", language); got != "Memory cleared." {
			t.Errorf("Translate(commands.reset, %q) = %q, want %q", language, got, "Memory cleared.")
		}
	}
}

func TestTranslateFallsBackToDefault(t *testing.T) {
	if err := LoadTranslations(writeLanguages(t)); err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}

	// "only_en" is missing from RU.json, so it must fall back to EN.
	if got := Translate("only_en", "RU"); got != "english only" {
		t.Errorf("Translate(only_en, RU) = %q, want %q", got, "english only")
	}

	// An unknown language falls back instead of returning the raw key.
	if got := Translate("commands.reset", "Klingon"); got != "Memory cleared." {
		t.Errorf("Translate with unknown language = %q, want %q", got, "Memory cleared.")
	}

	// A genuinely missing key still returns the key so it is visible.
	if got := Translate("nope.missing", "EN"); got != "nope.missing" {
		t.Errorf("Translate(missing) = %q, want the key itself", got)
	}
}

// TestConcurrentLoadAndTranslate fails under -race if LoadTranslations
// replaces the bundle without synchronisation while Translate reads it.
func TestConcurrentLoadAndTranslate(t *testing.T) {
	dir := writeLanguages(t)

	if err := LoadTranslations(dir); err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}

	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = Translate("commands.reset", "EN")
			}
		}()
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := LoadTranslations(dir); err != nil {
				t.Errorf("LoadTranslations: %v", err)
			}
		}()
	}

	wg.Wait()
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"en":    "EN",
		" EN ":  "EN",
		"ru":    "RU",
		"":      "",
		"eN_GB": "EN_GB",
	}

	for input, want := range cases {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}
