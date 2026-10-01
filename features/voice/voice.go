// Package voice provides modular speech-to-text (STT) and optional
// text-to-speech (TTS) over OpenAI-compatible audio endpoints.
package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"openrouter-bot/internal/security"
	"openrouter-bot/provider"
)

// ErrVoiceUnavailable is returned when no speech-to-text provider is configured.
var ErrVoiceUnavailable = errors.New("no speech-to-text provider configured (set GROQ_API_KEY or VOICE_STT_API_KEY)")

// maxAudioBytes bounds voice uploads to 15 MiB.
const maxAudioBytes = 15 << 20

type sttEndpoint struct {
	baseURL string
	apiKey  string
	model   string
}

func resolveSTTEndpoint(chain *provider.Chain) (sttEndpoint, bool) {
	if base := strings.TrimSpace(os.Getenv("VOICE_STT_BASE_URL")); base != "" {
		key := strings.TrimSpace(os.Getenv("VOICE_STT_API_KEY"))
		model := strings.TrimSpace(os.Getenv("VOICE_STT_MODEL"))
		if model == "" {
			model = "whisper-large-v3-turbo"
		}
		if key != "" {
			return sttEndpoint{baseURL: strings.TrimRight(base, "/"), apiKey: key, model: model}, true
		}
	}

	if groqKey := strings.TrimSpace(os.Getenv("GROQ_API_KEY")); groqKey != "" {
		return sttEndpoint{
			baseURL: "https://api.groq.com/openai/v1",
			apiKey:  groqKey,
			model:   "whisper-large-v3-turbo",
		}, true
	}

	if chain != nil {
		for _, info := range chain.Status() {
			if strings.EqualFold(info.Name, "groq") && info.Configured {
				if models, ok := chain.ModelsOf("groq"); ok && len(models) > 0 {
					// Look up the Groq API key via environment preset if present.
					if preset, ok := provider.LookupPreset("groq"); ok && preset.APIKeyEnv != "" {
						if k := strings.TrimSpace(os.Getenv(preset.APIKeyEnv)); k != "" {
							return sttEndpoint{
								baseURL: strings.TrimRight(info.BaseURL, "/"),
								apiKey:  k,
								model:   "whisper-large-v3-turbo",
							}, true
						}
					}
				}
			}
		}
	}

	return sttEndpoint{}, false
}

// Available reports whether a speech-to-text backend is configured.
func Available(chain *provider.Chain) bool {
	_, ok := resolveSTTEndpoint(chain)
	return ok
}

// Transcribe converts voice audio bytes into text using the configured Whisper
// / OpenAI-compatible audio transcription endpoint.
func Transcribe(ctx context.Context, chain *provider.Chain, audioData []byte, filename string) (string, error) {
	if len(audioData) == 0 {
		return "", errors.New("empty audio data")
	}
	if len(audioData) > maxAudioBytes {
		return "", errors.New("audio file exceeds maximum allowed size")
	}

	ep, ok := resolveSTTEndpoint(chain)
	if !ok {
		return "", ErrVoiceUnavailable
	}

	return TranscribeWithEndpoint(ctx, ep.baseURL, ep.apiKey, ep.model, audioData, filename)
}

// TranscribeWithEndpoint sends a multipart transcription request to baseURL.
func TranscribeWithEndpoint(
	ctx context.Context,
	baseURL, apiKey, model string,
	audioData []byte,
	filename string,
) (string, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return "", ErrVoiceUnavailable
	}
	if model == "" {
		model = "whisper-large-v3-turbo"
	}
	filename = security.SanitizeFilename(filename)
	if filename == "" || filename == "file" {
		filename = "voice.ogg"
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(audioData); err != nil {
		return "", err
	}
	if err := writer.WriteField("model", model); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := provider.SharedHTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("transcription API returned status %d", resp.StatusCode)
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", err
	}

	text := strings.TrimSpace(parsed.Text)
	if text == "" {
		return "", errors.New("transcription returned empty text")
	}
	return text, nil
}
