// Package lang provides a minimal, thread-safe translation store.
//
// Translation files are plain JSON documents named after the language they
// contain, for example EN.json or RU.json. A language is therefore added by
// dropping a new file into the directory - no code change is required.
package lang

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// DefaultLanguage is used whenever the requested language is unknown or empty.
const DefaultLanguage = "EN"

var (
	// mu guards translations. The map is replaced wholesale when translations
	// are reloaded, so readers must never observe a partially built map.
	mu           sync.RWMutex
	translations = make(map[string]map[string]interface{})
)

// LoadTranslations reads every *.json file in langDir and atomically replaces
// the active translation set. It is safe to call concurrently with Translate
// and safe to call more than once.
func LoadTranslations(langDir string) error {
	entries, err := os.ReadDir(langDir)
	if err != nil {
		return err
	}

	loaded := make(map[string]map[string]interface{})

	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}

		code := Normalize(strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
		if code == "" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(langDir, entry.Name()))
		if err != nil {
			return err
		}

		var parsed map[string]interface{}
		if err := json.Unmarshal(data, &parsed); err != nil {
			return err
		}

		loaded[code] = parsed
	}

	if len(loaded) == 0 {
		return &NoTranslationsError{Dir: langDir}
	}

	mu.Lock()
	translations = loaded
	mu.Unlock()

	codes := Languages()
	log.Printf("Loaded %d translation file(s) from %s: %v", len(codes), langDir, codes)

	return nil
}

// Normalize turns arbitrary user or configuration input ("en", " en ", "EN")
// into the canonical upper-case language code used as a map key.
func Normalize(language string) string {
	return strings.ToUpper(strings.TrimSpace(language))
}

// Languages returns the sorted list of currently available language codes.
func Languages() []string {
	mu.RLock()
	defer mu.RUnlock()

	return languagesLocked()
}

// languagesLocked reads the language codes without locking. Callers must hold
// mu for reading. It exists because sync.RWMutex is not reentrant: taking a
// second read lock while a writer is waiting deadlocks the process.
func languagesLocked() []string {
	codes := make([]string, 0, len(translations))
	for code := range translations {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	return codes
}

// Translate resolves a dotted key such as "commands.start" for the given
// language.
//
// Resolution order:
//
//  1. the requested language (normalised to upper case)
//  2. the default language
//  3. any available language
//  4. the key itself, so a missing translation is visible rather than silent
func Translate(key string, language string) string {
	mu.RLock()
	defer mu.RUnlock()

	for _, code := range candidates(language) {
		if value, ok := lookup(code, key); ok {
			return value
		}
	}

	return key
}

// Has reports whether a key exists for the given language.
func Has(key string, language string) bool {
	mu.RLock()
	defer mu.RUnlock()

	_, ok := lookup(Normalize(language), key)

	return ok
}

// candidates returns the languages to try, in priority order.
//
// It must only be called while mu is held for reading.
func candidates(language string) []string {
	requested := Normalize(language)

	order := make([]string, 0, len(translations)+1)
	if requested != "" {
		order = append(order, requested)
	}
	if requested != DefaultLanguage {
		order = append(order, DefaultLanguage)
	}
	for _, code := range languagesLocked() {
		if code != requested && code != DefaultLanguage {
			order = append(order, code)
		}
	}

	return order
}

func lookup(language, key string) (string, bool) {
	bundle, ok := translations[language]
	if !ok {
		return "", false
	}

	var value interface{} = bundle
	for _, part := range strings.Split(key, ".") {
		node, ok := value.(map[string]interface{})
		if !ok {
			return "", false
		}
		value, ok = node[part]
		if !ok {
			return "", false
		}
	}

	str, ok := value.(string)

	return str, ok
}

// NoTranslationsError is returned when a translation directory contains no
// usable JSON files.
type NoTranslationsError struct {
	Dir string
}

func (e *NoTranslationsError) Error() string {
	return "no translation files found in " + e.Dir
}
