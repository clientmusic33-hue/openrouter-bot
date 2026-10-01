package api

import (
	"strings"
	"testing"

	"openrouter-bot/config"
	"openrouter-bot/provider"
)

func TestSplitMessageKeepsShortText(t *testing.T) {
	got := SplitMessage("hello world", 3800, 0)
	if len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("SplitMessage = %#v, want a single chunk", got)
	}
}

func TestSplitMessageRespectsLimit(t *testing.T) {
	text := strings.Repeat("a", 1000)

	got := SplitMessage(text, 300, 0)

	if len(got) != 4 {
		t.Fatalf("SplitMessage produced %d chunks, want 4", len(got))
	}
	for i, chunk := range got {
		if len([]rune(chunk)) > 300 {
			t.Errorf("chunk %d has %d runes, want <= 300", i, len([]rune(chunk)))
		}
	}
	if strings.Join(got, "") != text {
		t.Errorf("chunks do not reassemble into the original text")
	}
}

func TestSplitMessagePrefersNewlines(t *testing.T) {
	// Two 10-character paragraphs separated by a newline.
	text := strings.Repeat("a", 10) + "\n" + strings.Repeat("b", 10)

	got := SplitMessage(text, 15, 0)

	if len(got) != 2 {
		t.Fatalf("SplitMessage = %#v, want 2 chunks", got)
	}
	if got[0] != strings.Repeat("a", 10) || got[1] != strings.Repeat("b", 10) {
		t.Errorf("SplitMessage = %#v, want a paragraph per chunk", got)
	}
}

// TestSplitMessageMultibyte guards against splitting in the middle of a
// multi-byte rune, which would produce replacement characters.
func TestSplitMessageMultibyte(t *testing.T) {
	text := strings.Repeat("😀", 100)

	got := SplitMessage(text, 50, 0)

	reassembled := strings.Join(got, "")
	if !strings.HasPrefix(reassembled, strings.Repeat("😀", 99)) {
		t.Errorf("multibyte runes were corrupted: reassembled %d runes", len([]rune(reassembled)))
	}
	for _, chunk := range got {
		if strings.ContainsRune(chunk, '�') {
			t.Fatalf("chunk contains a replacement character: %q", chunk)
		}
	}
}

func TestSplitMessageCapsChunks(t *testing.T) {
	text := strings.Repeat("x", 1000)

	got := SplitMessage(text, 100, 3)

	if len(got) != 3 {
		t.Fatalf("SplitMessage produced %d chunks, want at most 3", len(got))
	}
	if !strings.Contains(got[len(got)-1], "truncated") {
		t.Errorf("last chunk should be marked truncated, got %q", got[len(got)-1])
	}
}

func TestSplitMessageEmpty(t *testing.T) {
	if got := SplitMessage("   ", 100, 0); got != nil {
		t.Errorf("SplitMessage(blank) = %#v, want nil", got)
	}
}

func TestIsFreeModel(t *testing.T) {
	cases := []struct {
		name  string
		model Model
		want  bool
	}{
		{"zero strings", Model{Pricing: Pricing{Prompt: "0", Completion: "0"}}, true},
		{"empty means free", Model{Pricing: Pricing{}}, true},
		{"decimal zero", Model{Pricing: Pricing{Prompt: "0.0", Completion: "0.0"}}, true},
		{"paid completion", Model{Pricing: Pricing{Prompt: "0", Completion: "0.000002"}}, false},
		{"paid prompt", Model{Pricing: Pricing{Prompt: "0.000001", Completion: "0"}}, false},
	}

	for _, tc := range cases {
		if got := isFreeModel(tc.model); got != tc.want {
			t.Errorf("%s: isFreeModel() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFooterNamesModelAndFailover covers the "which model answered" line,
// including the visible note when the chain had to switch.
func TestFooterNamesModelAndFailover(t *testing.T) {
	candidate := provider.Candidate{Provider: "groq", Model: "llama-3.3-70b-versatile"}

	plain := footer(candidate, 0)
	if !strings.Contains(plain, "groq") || !strings.Contains(plain, "llama-3.3-70b-versatile") {
		t.Errorf("footer = %q, want provider and model", plain)
	}
	if strings.Contains(plain, "switched") {
		t.Errorf("footer = %q, should not mention a switch", plain)
	}

	switched := footer(candidate, 2)
	if !strings.Contains(switched, "2 failed attempt") {
		t.Errorf("footer = %q, want the failover note", switched)
	}
}

// TestSplitMessageDefaultLimit guards the persona cap: an answer that reaches
// the configured maximum stays inside one Telegram message.
func TestSplitMessageDefaultLimit(t *testing.T) {
	text := strings.Repeat("a", config.DefaultMaxReplyChars)

	chunks := SplitMessage(text, config.DefaultMaxReplyChars, 0)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want a single message", len(chunks))
	}
	if len([]rune(chunks[0])) > config.TelegramMaxMessageLength {
		t.Fatalf("chunk is %d runes, Telegram rejects more than %d",
			len([]rune(chunks[0])), config.TelegramMaxMessageLength)
	}
}
