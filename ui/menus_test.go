package ui

import (
	"strings"
	"testing"

	"openrouter-bot/provider"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// TestCallbackRoundTrip is the contract between the buttons and the handler:
// whatever a builder encodes, ParseCallback must decode.
func TestCallbackRoundTrip(t *testing.T) {
	cases := []struct {
		button tgbotapi.InlineKeyboardButton
		action string
	}{
		{tgbotapi.NewInlineKeyboardButtonData("menu", Encode(ActionMenu)), ActionMenu},
		{tgbotapi.NewInlineKeyboardButtonData("prov", Encode(ActionProvider, "3")), ActionProvider},
		{tgbotapi.NewInlineKeyboardButtonData("model", Encode(ActionModel, "1", "2")), ActionModel},
		{tgbotapi.NewInlineKeyboardButtonData("tog", Encode(ActionToggle, "footer")), ActionToggle},
		{tgbotapi.NewInlineKeyboardButtonData("fb", Encode(ActionFeedback, "down")), ActionFeedback},
	}

	for _, tc := range cases {
		if tc.button.CallbackData == nil {
			t.Fatalf("%s: button has no callback data", tc.action)
		}
		data := *tc.button.CallbackData

		if len(data) > 64 {
			t.Errorf("%s: callback data is %d bytes", tc.action, len(data))
		}

		callback, ok := ParseCallback(data)
		if !ok {
			t.Fatalf("%s: ParseCallback failed", tc.action)
		}
		if callback.Action != tc.action {
			t.Errorf("action = %q, want %q", callback.Action, tc.action)
		}
	}
}

func TestParseCallbackRejectsForeignData(t *testing.T) {
	for _, data := range []string{"", "hello", "cb:", "cb:a:b:c:d:e"} {
		if _, ok := ParseCallback(data); ok {
			t.Errorf("ParseCallback(%q) should fail", data)
		}
	}
}

// TestEncodeFallsBackToNoop keeps a too long payload from being sent: an
// oversized callback is rejected by Telegram with a 400.
func TestEncodeFallsBackToNoop(t *testing.T) {
	data := Encode("x", strings.Repeat("a", 100))

	callback, ok := ParseCallback(data)
	if !ok || callback.Action != ActionNoop {
		t.Fatalf("oversized data should degrade to noop, got %q", data)
	}
}

func TestUseModelButton(t *testing.T) {
	button, ok := UseModelButton("llama-3.3-70b-versatile")
	if !ok {
		t.Fatal("a normal model id must produce a button")
	}

	if button.CallbackData == nil {
		t.Fatal("button has no callback data")
	}

	callback, ok := ParseCallback(*button.CallbackData)
	if !ok || callback.Action != ActionUse {
		t.Fatalf("callback = %#v", callback)
	}
	if got := ModelFromCallback(callback); got != "llama-3.3-70b-versatile" {
		t.Errorf("ModelFromCallback = %q", got)
	}

	if _, ok := UseModelButton(""); ok {
		t.Error("an empty model must not produce a button")
	}
	if _, ok := UseModelButton(strings.Repeat("m", 80)); ok {
		t.Error("a model that cannot fit in callback data must not produce a button")
	}
}

func TestArgsToIndices(t *testing.T) {
	indices, ok := ArgsToIndices("2", "0")
	if !ok || len(indices) != 2 || indices[0] != 2 || indices[1] != 0 {
		t.Fatalf("ArgsToIndices = %v, %v", indices, ok)
	}

	for _, bad := range [][]string{{"-1"}, {"x"}, {"999999"}, {}} {
		if _, ok := ArgsToIndices(bad...); ok {
			t.Errorf("ArgsToIndices(%v) should fail", bad)
		}
	}
}

// TestProviderListMarksState checks the two things a user needs to see at a
// glance: which provider is active and which one has no key.
func TestProviderListMarksState(t *testing.T) {
	infos := []provider.Info{
		{Name: "openrouter", Configured: true, Models: []string{"a"}},
		{Name: "groq", Configured: false, Models: []string{"b"}},
	}

	markup := ProviderList(infos, "openrouter", 0, 6)

	var labels []string
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			labels = append(labels, button.Text)
		}
	}

	joined := strings.Join(labels, "|")
	if !strings.Contains(joined, "▶ openrouter") {
		t.Errorf("the active provider is not marked: %q", joined)
	}
	if !strings.Contains(joined, "groq (no key)") {
		t.Errorf("a keyless provider is not marked: %q", joined)
	}
}
