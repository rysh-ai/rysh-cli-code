package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// TestNew_NeverSilentlyReturnsAMock is the regression guard for defect 9.
//
// New() used to match only the exact string "claude" and route everything else
// — "ollama", "openai", "anthropic" (the name the onboarding flow writes) and
// the empty default — to a StaticProvider whose canned text was returned as a
// SUCCESSFUL completion. The loop-engineering judge seat
// (workspace_auto_loop.go) is the live consumer, so under any of those settings
// it scored iterations against a fixed string with no error and no log.
//
// The selector must now mirror NewAgenticProvider exactly.
func TestNew_NeverSilentlyReturnsAMock(t *testing.T) {
	tests := []struct {
		providerName string
		wantName     string
	}{
		{"ollama", "ollama"},
		{"  Ollama  ", "ollama"}, // trimmed + case-folded, as the agentic selector does
		{"openai", "openai"},
		{"claude", "claude"},            // no key ⇒ CLI adapter
		{"anthropic", "claude"},         // the onboarding name — used to mock
		{"", "claude"},                  // unset default — used to mock
		{"claude-agentic", "claude"},    // runtime alias — used to mock
		{"something-unknown", "claude"}, // unknown falls back to Claude, never to a mock
	}

	for _, tc := range tests {
		t.Run(tc.providerName, func(t *testing.T) {
			p := New(config.Config{ProviderName: tc.providerName})
			if got := p.Name(); got != tc.wantName {
				t.Fatalf("New(%q).Name() = %q, want %q", tc.providerName, got, tc.wantName)
			}
			if _, isMock := p.(*StaticProvider); isMock {
				t.Fatalf("New(%q) returned a StaticProvider — a mock must never be a silent default",
					tc.providerName)
			}
		})
	}
}

// TestNew_MirrorsAgenticSelector pins the two selectors together. If someone
// changes one switch without the other, defect 9 comes back.
func TestNew_MirrorsAgenticSelector(t *testing.T) {
	names := []string{"", "claude", "anthropic", "claude-agentic", "openai", "ollama", "weird"}
	for _, n := range names {
		cfg := config.Config{ProviderName: n}
		// Both providers name themselves after the dialect they speak, so a
		// "claude" prefix is the reliable discriminator without reaching for
		// unexported types.
		simpleIsOpenAI := !strings.HasPrefix(New(cfg).Name(), "claude")
		agenticIsOpenAI := !strings.HasPrefix(NewAgenticProvider(cfg).Name(), "claude")

		if simpleIsOpenAI != agenticIsOpenAI {
			t.Fatalf("selectors diverge for %q: simple openai=%v, agentic openai=%v",
				n, simpleIsOpenAI, agenticIsOpenAI)
		}
	}
}

// TestNew_OpenAICompatibleActuallyCallsTheEndpoint proves the judge seat now
// reaches a real provider: the fake OpenAI-compatible server must be hit and
// its answer returned verbatim.
func TestNew_OpenAICompatibleActuallyCallsTheEndpoint(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if !strings.Contains(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{{
				"message":       map[string]interface{}{"role": "assistant", "content": "VERDICT: pass"},
				"finish_reason": "stop",
			}},
			"usage": map[string]interface{}{"prompt_tokens": 5, "completion_tokens": 3},
		})
	}))
	defer srv.Close()

	p := New(config.Config{
		ProviderName: "ollama",
		APIURL:       srv.URL,
		DefaultModel: "llama3.1",
		MaxTokens:    256,
	})

	got, err := p.Complete(context.Background(), "score this iteration")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected exactly one call to the provider, got %d", hits)
	}
	if got != "VERDICT: pass" {
		t.Fatalf("judge must receive the provider's real answer, got %q", got)
	}
	if strings.Contains(got, "provider not configured") {
		t.Fatal("the mock's canned text leaked into a judge verdict")
	}
}
