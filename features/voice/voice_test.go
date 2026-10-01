package voice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTranscribeGracefullyFailsWithoutCredentials(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv("VOICE_STT_BASE_URL", "")
	t.Setenv("VOICE_STT_API_KEY", "")

	if Available(nil) {
		t.Fatal("expected voice to be unavailable without keys")
	}

	_, err := Transcribe(context.Background(), nil, []byte("ogg-bytes"), "voice.ogg")
	if !errors.Is(err, ErrVoiceUnavailable) {
		t.Fatalf("expected ErrVoiceUnavailable, got %v", err)
	}
}

func TestTranscribeWithEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello from voice"}`))
	}))
	defer srv.Close()

	got, err := TranscribeWithEndpoint(context.Background(), srv.URL, "test-key", "whisper-large-v3-turbo", []byte("fake-audio"), "note.ogg")
	if err != nil {
		t.Fatalf("TranscribeWithEndpoint: %v", err)
	}
	if got != "hello from voice" {
		t.Errorf("got %q, want %q", got, "hello from voice")
	}
}
