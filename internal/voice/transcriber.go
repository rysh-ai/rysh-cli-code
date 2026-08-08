package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Transcriber turns a recorded audio file into text.
type Transcriber interface {
	Transcribe(ctx context.Context, audioPath string) (string, error)
	// Name reports the provider name (e.g. "deepgram", "whisper").
	Name() string
}

// NewTranscriber builds a Transcriber for the configured provider. provider is
// matched case-insensitively: "deepgram" (default) and "whisper"/"openai" are
// supported. apiKey is the credential for the selected provider; language is an
// optional BCP-47 hint (empty = auto-detect).
func NewTranscriber(provider, apiKey, language string) (Transcriber, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "deepgram":
		return &deepgramTranscriber{
			apiKey:   apiKey,
			model:    "nova-3",
			language: language,
			client:   defaultHTTPClient(),
		}, nil
	case "whisper", "openai":
		return &openAIWhisperTranscriber{
			apiKey:   apiKey,
			model:    "whisper-1",
			language: language,
			client:   defaultHTTPClient(),
		}, nil
	default:
		return nil, fmt.Errorf("voice: unknown tts_provider_name %q (supported: deepgram, whisper)", provider)
	}
}

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second}
}

// audioContentType maps a recording's file extension to its MIME type for
// providers that take raw audio bytes (Deepgram). The TUI's own recorder
// always produces WAV, which stays the default; browser MediaRecorder uploads
// (web voice, roadmap W10) arrive as webm/ogg/mp4 and must be declared as
// such or the provider mis-sniffs the container.
func audioContentType(audioPath string) string {
	switch strings.ToLower(filepath.Ext(audioPath)) {
	case ".webm":
		return "audio/webm"
	case ".ogg":
		return "audio/ogg"
	case ".mp4", ".m4a":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	default:
		return "audio/wav"
	}
}

// ---------------------------------------------------------------------------
// Deepgram (default)
// ---------------------------------------------------------------------------

const deepgramListenURL = "https://api.deepgram.com/v1/listen"

type deepgramTranscriber struct {
	apiKey   string
	model    string
	language string
	baseURL  string // overridable in tests; empty = deepgramListenURL
	client   *http.Client
}

func (t *deepgramTranscriber) Name() string { return "deepgram" }

func (t *deepgramTranscriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	if t.apiKey == "" {
		return "", fmt.Errorf("voice: deepgram api_key is not set")
	}
	audio, err := os.ReadFile(audioPath)
	if err != nil {
		return "", fmt.Errorf("voice: read audio: %w", err)
	}

	base := t.baseURL
	if base == "" {
		base = deepgramListenURL
	}
	q := url.Values{}
	q.Set("model", t.model)
	q.Set("smart_format", "true")
	q.Set("punctuate", "true")
	if t.language != "" {
		q.Set("language", t.language)
	}
	endpoint := base + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(audio))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Token "+t.apiKey)
	req.Header.Set("Content-Type", audioContentType(audioPath))

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("voice: deepgram request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("voice: deepgram returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("voice: parse deepgram response: %w", err)
	}
	if len(parsed.Results.Channels) == 0 || len(parsed.Results.Channels[0].Alternatives) == 0 {
		return "", nil
	}
	return strings.TrimSpace(parsed.Results.Channels[0].Alternatives[0].Transcript), nil
}

// ---------------------------------------------------------------------------
// OpenAI Whisper
// ---------------------------------------------------------------------------

const openAITranscriptionsURL = "https://api.openai.com/v1/audio/transcriptions"

type openAIWhisperTranscriber struct {
	apiKey   string
	model    string
	language string
	baseURL  string // overridable in tests; empty = openAITranscriptionsURL
	client   *http.Client
}

func (t *openAIWhisperTranscriber) Name() string { return "whisper" }

func (t *openAIWhisperTranscriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	if t.apiKey == "" {
		return "", fmt.Errorf("voice: openai api_key is not set")
	}
	audio, err := os.ReadFile(audioPath)
	if err != nil {
		return "", fmt.Errorf("voice: read audio: %w", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(audio); err != nil {
		return "", err
	}
	_ = mw.WriteField("model", t.model)
	if t.language != "" {
		_ = mw.WriteField("language", t.language)
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	base := t.baseURL
	if base == "" {
		base = openAITranscriptionsURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("voice: openai request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("voice: openai returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("voice: parse openai response: %w", err)
	}
	return strings.TrimSpace(parsed.Text), nil
}
