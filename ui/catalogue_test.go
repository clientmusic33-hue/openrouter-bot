package ui

import (
	"strings"
	"testing"

	"openrouter-bot/provider"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func catalogueEntries(count int) []CatalogueEntry {
	entries := make([]CatalogueEntry, 0, count)
	for i := 0; i < count; i++ {
		entries = append(entries, CatalogueEntry{
			Ref: provider.ModelRef{
				Provider:     "openrouter",
				Model:        "meta-llama/llama-3.3-70b-instruct:free",
				DisplayName:  "Llama 3.3 70B Instruct",
				Availability: provider.ModelAvailable,
				PriceKnown:   true,
				Free:         true,
			},
			ProviderIndex: i % 3,
			ModelIndex:    i,
		})
	}

	return entries
}

// callbackData unwraps a button's payload; URL buttons simply have none.
func callbackData(button tgbotapi.InlineKeyboardButton) string {
	if button.CallbackData == nil {
		return ""
	}

	return *button.CallbackData
}

func allButtons(markups ...tgbotapi.InlineKeyboardMarkup) []tgbotapi.InlineKeyboardButton {
	var buttons []tgbotapi.InlineKeyboardButton
	for _, markup := range markups {
		for _, row := range markup.InlineKeyboard {
			buttons = append(buttons, row...)
		}
	}

	return buttons
}

// TestCatalogueCallbackDataStaysWithinTelegramLimit guards the 64 byte cap on
// every button of the new catalogue keyboards.
func TestCatalogueCallbackDataStaysWithinTelegramLimit(t *testing.T) {
	filter := provider.FilterFromBits(
		provider.FilterFree|provider.FilterWorking|provider.FilterLongContext|provider.FilterFavourites,
		[]string{"some-model"}, "",
	)

	markups := []tgbotapi.InlineKeyboardMarkup{
		Catalogue(filter, 128),
		CatalogueList(filter, catalogueEntries(20), "some-model", 0, 8, nil),
		ModelDetails("meta-llama/llama-3.3-70b-instruct:free", 12, 340, true),
		ModelList(4, "openrouter", []provider.ModelRef{{Model: "a", Provider: "openrouter"}}, "", 0, 6, nil),
	}

	for _, markup := range markups {
		for _, button := range allButtons(markup) {
			if len(callbackData(button)) > 64 {
				t.Errorf("callback data for %q is %d bytes", button.Text, len(callbackData(button)))
			}
		}
	}
}

// TestCatalogueFiltersToggleOneBit checks that every filter button flips
// exactly its own bit and marks the active ones.
func TestCatalogueFiltersToggleOneBit(t *testing.T) {
	bits := provider.FilterFree | provider.FilterVision
	keyboard := Catalogue(provider.FilterFromBits(bits, nil, ""), 10)

	labels := map[string]int{
		"🆓 Free":       provider.FilterFree,
		"🟢 Working":    provider.FilterWorking,
		"⚡ Fast":       provider.FilterFast,
		"🧠 Reasoning":  provider.FilterReasoning,
		"💻 Coding":     provider.FilterCoding,
		"👁 Vision":     provider.FilterVision,
		"📚 Long ctx":   provider.FilterLongContext,
		"⭐ Favourites": provider.FilterFavourites,
	}

	toggles := 0
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			callback, ok := ParseCallback(callbackData(button))
			if !ok || callback.Action != ActionCatalog {
				continue
			}

			name := strings.TrimPrefix(button.Text, "✅ ")
			bit, known := labels[name]
			if !known {
				continue
			}
			toggles++

			indices, ok := ArgsToIndices(callback.Args...)
			if !ok || len(indices) != 2 {
				t.Fatalf("filter callback %q must carry bits and page", callbackData(button))
			}

			if indices[0] != bits^bit {
				t.Errorf("%s toggled to %#b, want %#b", name, indices[0], bits^bit)
			}
			if active := strings.HasPrefix(button.Text, "✅"); active != (bits&bit != 0) {
				t.Errorf("%s is marked active=%v for bits %#b", name, active, bits)
			}
		}
	}

	if toggles != len(labels) {
		t.Errorf("expected %d filter toggles, got %d", len(labels), toggles)
	}
}

// TestCatalogueListPaginates keeps the list short enough for one message and
// marks the page.
func TestCatalogueListPaginates(t *testing.T) {
	keyboard := CatalogueList(provider.ModelFilter{}, catalogueEntries(20), "", 8, 8, nil)

	var models, pages int
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			callback, ok := ParseCallback(callbackData(button))
			if !ok {
				continue
			}
			switch callback.Action {
			case ActionModelInfo:
				models++
				indices, ok := ArgsToIndices(callback.Args...)
				if !ok || len(indices) != 2 {
					t.Fatalf("a model button must carry provider and model index: %q", callbackData(button))
				}
			case ActionNoop:
				if strings.Contains(button.Text, "/") {
					pages++
					if button.Text != "2/3" {
						t.Errorf("page indicator = %q, want 2/3", button.Text)
					}
				}
			}
		}
	}

	if models != 8 {
		t.Errorf("page shows %d models, want 8", models)
	}
	if pages != 1 {
		t.Errorf("page indicator missing")
	}
}

// TestModelDetailsCarriesTheModelAndItsIndices covers the details buttons.
func TestModelDetailsCarriesTheModelAndItsIndices(t *testing.T) {
	keyboard := ModelDetails("qwen/qwen3-coder:free", 2, 17, false)

	actions := map[string][]string{}
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			callback, ok := ParseCallback(callbackData(button))
			if !ok {
				continue
			}
			actions[callback.Action] = callback.Args
		}
	}

	if got := actions[ActionUse]; strings.Join(got, ":") != "qwen/qwen3-coder:free" {
		t.Errorf("the use button lost the model id: %v", got)
	}
	if got := actions[ActionFavouriteModel]; len(got) != 2 || got[0] != "2" || got[1] != "17" {
		t.Errorf("the favourite button lost the indices: %v", got)
	}
	if _, ok := actions[ActionProviderRefresh]; !ok {
		t.Error("the details screen has no refresh button")
	}
	if got := actions[ActionProvider]; len(got) != 1 || got[0] != "2" {
		t.Errorf("the back button lost the provider index: %v", got)
	}
}

// TestUseModelButtonKeepsFreeVariants pins the ":free" ids, which is where a
// model id and the callback separator collide.
func TestUseModelButtonKeepsFreeVariants(t *testing.T) {
	button, ok := UseModelButton("deepseek/deepseek-r1:free")
	if !ok || button.CallbackData == nil {
		t.Fatal("the use button was dropped")
	}

	callback, parsed := ParseCallback(*button.CallbackData)
	if !parsed {
		t.Fatal("ParseCallback failed")
	}
	if got := ModelFromCallback(callback); got != "deepseek/deepseek-r1:free" {
		t.Errorf("model id = %q", got)
	}
}

// TestModelDetailsSkipsAnOversizedModelId keeps Telegram from rejecting a
// button whose payload would not fit.
func TestModelDetailsSkipsAnOversizedModelId(t *testing.T) {
	keyboard := ModelDetails(strings.Repeat("very-long-model-id/", 6), 0, 0, false)

	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			callback, ok := ParseCallback(callbackData(button))
			if !ok {
				continue
			}
			if callback.Action == ActionUse {
				t.Error("a model id that cannot fit must not get a use button")
			}
		}
	}
}
