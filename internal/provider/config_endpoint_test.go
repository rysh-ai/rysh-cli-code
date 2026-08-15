// SPDX-License-Identifier: Apache-2.0

package provider

// Which HOST does a config-level `provider.name:` selection actually talk to?
//
// Every Config starts with api_url = https://api.anthropic.com, because the
// default provider is Claude. The OpenAI-family constructor only substituted
// its own default when api_url was EMPTY, which it never is after config
// loading — so `provider.name: openai` in rysh.config.yaml sent OpenAI traffic
// to Anthropic's host and every turn died on a bare 404. Ollama (air-gapped
// mode) and Gemini inherited the same fault.
//
// The ##pane / ##humanoid override paths were unaffected because they blank
// APIURL before constructing, which is exactly why this needed pinning: the
// bug was invisible from the code path the feature was developed against.

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// TestOpenAIFamilyDefaults_Endpoint pins the endpoint each family resolves to
// from a fully-defaulted config, and that an explicit api_url still wins.
func TestOpenAIFamilyDefaults_Endpoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		apiURL   string // as it appears in Config (defaulted or explicit)
		want     string
	}{
		{
			name:     "openai keeps its own host",
			provider: "openai",
			apiURL:   config.DefaultAnthropicAPIURL,
			want:     "https://api.openai.com/v1",
		},
		{
			name:     "ollama keeps loopback (air-gapped mode)",
			provider: "ollama",
			apiURL:   config.DefaultAnthropicAPIURL,
			want:     "http://127.0.0.1:11434/v1",
		},
		{
			name:     "gemini keeps its compat surface",
			provider: "gemini",
			apiURL:   config.DefaultAnthropicAPIURL,
			want:     GeminiAPIURL,
		},
		{
			name:     "an explicit api_url still wins",
			provider: "openai",
			apiURL:   "https://proxy.internal/v1",
			want:     "https://proxy.internal/v1",
		},
		{
			name:     "an empty api_url takes the family default",
			provider: "openai",
			apiURL:   "",
			want:     "https://api.openai.com/v1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got, _ := openAIFamilyDefaults(tc.provider, tc.apiURL, "")
			if got != tc.want {
				t.Errorf("%s endpoint = %q, want %q", tc.provider, got, tc.want)
			}
			// Whatever the endpoint resolution does, it must never leave a
			// non-Claude family pointed at Anthropic.
			if got == config.DefaultAnthropicAPIURL {
				t.Errorf("%s resolved to the Anthropic host", tc.provider)
			}
		})
	}
}

// TestOpenAIFamilyDefaults_NameAndModel: the family name is normalized and an
// unrecognized name falls to OpenAI (the historical behaviour), each with its
// own default model unless the config pinned one.
func TestOpenAIFamilyDefaults_NameAndModel(t *testing.T) {
	for _, tc := range []struct {
		provider  string
		model     string
		wantName  string
		wantModel string
	}{
		{"openai", "", "openai", "gpt-4o"},
		{"  OpenAI ", "", "openai", "gpt-4o"},
		{"openai", "gpt-5.6-sol", "openai", "gpt-5.6-sol"},
		{"ollama", "", "ollama", "llama3.1"},
		{"gemini", "", "gemini", geminiDefaultModel},
		{"something-else", "", "openai", "gpt-4o"},
	} {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			name, _, model := openAIFamilyDefaults(tc.provider, "", tc.model)
			if name != tc.wantName || model != tc.wantModel {
				t.Errorf("= %q/%q, want %q/%q", name, model, tc.wantName, tc.wantModel)
			}
		})
	}
}
