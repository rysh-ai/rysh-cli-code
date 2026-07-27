package voice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func writeTempAudio(t *testing.T, data string) string {
	t.Helper()
	f, err := os.CreateTemp("", "rysh-voice-test-*.wav")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	_, _ = f.WriteString(data)
	_ = f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func TestNewTranscriberSelection(t *testing.T) {
	cases := []struct {
		provider string
		want     string
		wantErr  bool
	}{
		{"", "deepgram", false},
		{"deepgram", "deepgram", false},
		{"Deepgram", "deepgram", false},
		{"whisper", "whisper", false},
		{"openai", "whisper", false},
		{"bogus", "", true},
	}
	for _, tc := range cases {
		tr, err := NewTranscriber(tc.provider, "key", "")
		if tc.wantErr {
			if err == nil {
				t.Errorf("provider %q: expected error, got none", tc.provider)
			}
			continue
		}
		if err != nil {
			t.Errorf("provider %q: unexpected error: %v", tc.provider, err)
			continue
		}
		if tr.Name() != tc.want {
			t.Errorf("provider %q: got name %q, want %q", tc.provider, tr.Name(), tc.want)
		}
	}
}

func TestDeepgramTranscribe(t *testing.T) {
	var gotAuth, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if string(body) != "FAKEAUDIO" {
			t.Errorf("body = %q, want FAKEAUDIO", string(body))
		}
		if r.URL.Query().Get("model") != "nova-3" {
			t.Errorf("model = %q, want nova-3", r.URL.Query().Get("model"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"results":{"channels":[{"alternatives":[{"transcript":"hello world"}]}]}}`)
	}))
	defer srv.Close()

	tr := &deepgramTranscriber{apiKey: "secret", model: "nova-3", baseURL: srv.URL, client: srv.Client()}
	path := writeTempAudio(t, "FAKEAUDIO")
	got, err := tr.Transcribe(context.Background(), path)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got != "hello world" {
		t.Errorf("transcript = %q, want %q", got, "hello world")
	}
	if gotAuth != "Token secret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Token secret")
	}
	if gotContentType != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav", gotContentType)
	}
}

func TestDeepgramTranscribeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"err_msg":"bad key"}`)
	}))
	defer srv.Close()

	tr := &deepgramTranscriber{apiKey: "secret", model: "nova-3", baseURL: srv.URL, client: srv.Client()}
	path := writeTempAudio(t, "FAKEAUDIO")
	_, err := tr.Transcribe(context.Background(), path)
	if err == nil {
		t.Fatal("expected error on non-200, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to mention 401", err)
	}
}

func TestOpenAIWhisperTranscribe(t *testing.T) {
	var gotAuth string
	var sawFile, sawModel bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		if _, _, err := r.FormFile("file"); err == nil {
			sawFile = true
		}
		if r.FormValue("model") == "whisper-1" {
			sawModel = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"text":"transcribed text"}`)
	}))
	defer srv.Close()

	tr := &openAIWhisperTranscriber{apiKey: "sk-test", model: "whisper-1", baseURL: srv.URL, client: srv.Client()}
	path := writeTempAudio(t, "FAKEAUDIO")
	got, err := tr.Transcribe(context.Background(), path)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got != "transcribed text" {
		t.Errorf("transcript = %q, want %q", got, "transcribed text")
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sk-test")
	}
	if !sawFile {
		t.Error("expected multipart file field 'file'")
	}
	if !sawModel {
		t.Error("expected model field 'whisper-1'")
	}
}
