// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// TestProviderUsable_KeylessLocalProviderIsUsable is the regression guard for
// the bug that made `RYSH_PROVIDER=ollama rysh` return a canned mock string:
// the agentic setup gated the real provider on cfg.APIKey != "", but a local
// ollama endpoint needs no key, so the documented air-gapped path silently ran
// staticAgenticProvider{"mock-agentic"}.
func TestProviderUsable_KeylessLocalProviderIsUsable(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		apiKey   string
		want     bool
	}{
		{"ollama without a key is usable", "ollama", "", true},
		{"ollama is case/space insensitive", "  Ollama  ", "", true},
		{"ollama with a key is still usable", "ollama", "sk-unused", true},
		{"openai without a key falls back to mock", "openai", "", false},
		{"openai with a key is usable", "openai", "sk-test", true},
		{"anthropic without a key falls back to mock", "anthropic", "", false},
		{"anthropic with a key is usable", "anthropic", "sk-test", true},
		{"empty provider without a key falls back to mock", "", "", false},
		{"empty provider with a key is usable", "", "sk-test", true},
		// B6: claude-cli is keyless (the CLI carries its own login) — gating it
		// on a key would make the newly wired selection unreachable in practice.
		{"claude-cli without a key is usable", "claude-cli", "", true},
		// B6: gemini is a hosted API — keyless must fall back to the (loud) mock.
		{"gemini without a key falls back to mock", "gemini", "", false},
		{"gemini with a key is usable", "gemini", "g-key", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{ProviderName: tc.provider, APIKey: tc.apiKey}
			if got := providerUsable(cfg); got != tc.want {
				t.Fatalf("providerUsable(provider=%q, key=%q) = %v, want %v",
					tc.provider, tc.apiKey, got, tc.want)
			}
		})
	}
}
