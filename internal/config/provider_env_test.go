// SPDX-License-Identifier: Apache-2.0

package config

// Tests for the per-provider key sourcing in applyEnvOverrides (design 002
// §3.4, roadmap B6). The documented invocation is:
//
//	RYSH_PROVIDER=<name> <NAME>_API_KEY=... rysh
//
// with GOOGLE_API_KEY as Gemini's fallback variable. The dangerous cases are
// key CROSS-CONTAMINATION: an ambient ANTHROPIC_API_KEY must never be handed to
// OpenAI or Gemini (a foreign key just yields a confusing upstream 401), and
// RYSH_API_KEY stays the explicit override for every provider.

import (
	"slices"
	"testing"
)

// clearProviderKeyEnv blanks every variable applyEnvOverrides consults for the
// provider/key pair, so ambient developer environments can't skew the test.
func clearProviderKeyEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"RYSH_PROVIDER", "RYSH_API_KEY",
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OLLAMA_API_KEY",
		"GEMINI_API_KEY", "GOOGLE_API_KEY",
	} {
		t.Setenv(v, "")
	}
}

// TestApplyEnvOverrides_KeyVarPerProvider walks the whole table: each family
// takes its own conventional variable, and none of them inherits another
// family's key.
func TestApplyEnvOverrides_KeyVarPerProvider(t *testing.T) {
	for _, tc := range []struct {
		provider string
		setVar   string
		wantKey  string
		// ownVars are every variable this family may legitimately read, so the
		// cross-contamination case sets only the ones it must ignore.
		ownVars []string
	}{
		{"openai", "OPENAI_API_KEY", "oai-key", []string{"OPENAI_API_KEY"}},
		{"gemini", "GEMINI_API_KEY", "gem-key", []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
		{"ollama", "OLLAMA_API_KEY", "olm-key", []string{"OLLAMA_API_KEY"}},
		{"claude", "ANTHROPIC_API_KEY", "ant-key", []string{"ANTHROPIC_API_KEY"}},
		// An unset provider is the Claude default.
		{"", "ANTHROPIC_API_KEY", "ant-key", []string{"ANTHROPIC_API_KEY"}},
	} {
		name := tc.provider
		if name == "" {
			name = "(unset)"
		}
		t.Run(name, func(t *testing.T) {
			clearProviderKeyEnv(t)
			t.Setenv("RYSH_PROVIDER", tc.provider)
			t.Setenv(tc.setVar, tc.wantKey)
			if cfg := applyEnvOverrides(Config{}); cfg.APIKey != tc.wantKey {
				t.Fatalf("APIKey = %q, want %q from %s", cfg.APIKey, tc.wantKey, tc.setVar)
			}
		})

		t.Run(name+"/no foreign key", func(t *testing.T) {
			clearProviderKeyEnv(t)
			t.Setenv("RYSH_PROVIDER", tc.provider)
			// Every OTHER family's variable is set; this provider must pick up
			// none of them.
			for _, other := range []string{"OPENAI_API_KEY", "OLLAMA_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "ANTHROPIC_API_KEY"} {
				if !slices.Contains(tc.ownVars, other) {
					t.Setenv(other, "foreign-"+other)
				}
			}
			if cfg := applyEnvOverrides(Config{}); cfg.APIKey != "" {
				t.Fatalf("APIKey = %q — a foreign key was handed to %q", cfg.APIKey, tc.provider)
			}
		})
	}
}

func TestApplyEnvOverrides_GeminiKeySourcing(t *testing.T) {
	t.Run("GEMINI_API_KEY is the documented variable", func(t *testing.T) {
		clearProviderKeyEnv(t)
		t.Setenv("RYSH_PROVIDER", "gemini")
		t.Setenv("GEMINI_API_KEY", "g-key")
		t.Setenv("GOOGLE_API_KEY", "google-key") // GEMINI_API_KEY wins
		cfg := applyEnvOverrides(Config{})
		if cfg.ProviderName != "gemini" || cfg.APIKey != "g-key" {
			t.Fatalf("provider=%q key=%q, want gemini/g-key", cfg.ProviderName, cfg.APIKey)
		}
	})

	t.Run("GOOGLE_API_KEY is the fallback", func(t *testing.T) {
		clearProviderKeyEnv(t)
		t.Setenv("RYSH_PROVIDER", "gemini")
		t.Setenv("GOOGLE_API_KEY", "google-key")
		if cfg := applyEnvOverrides(Config{}); cfg.APIKey != "google-key" {
			t.Fatalf("APIKey = %q, want the GOOGLE_API_KEY fallback", cfg.APIKey)
		}
	})

	t.Run("RYSH_API_KEY overrides both", func(t *testing.T) {
		clearProviderKeyEnv(t)
		t.Setenv("RYSH_PROVIDER", "gemini")
		t.Setenv("RYSH_API_KEY", "explicit")
		t.Setenv("GEMINI_API_KEY", "g-key")
		if cfg := applyEnvOverrides(Config{}); cfg.APIKey != "explicit" {
			t.Fatalf("APIKey = %q, want the explicit RYSH_API_KEY", cfg.APIKey)
		}
	})

	t.Run("an anthropic key must not leak to gemini", func(t *testing.T) {
		clearProviderKeyEnv(t)
		t.Setenv("RYSH_PROVIDER", "gemini")
		t.Setenv("ANTHROPIC_API_KEY", "a-key")
		if cfg := applyEnvOverrides(Config{}); cfg.APIKey != "" {
			t.Fatalf("APIKey = %q — a foreign ANTHROPIC key was handed to gemini", cfg.APIKey)
		}
	})

	t.Run("claude family keeps the anthropic fallback", func(t *testing.T) {
		clearProviderKeyEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "a-key")
		if cfg := applyEnvOverrides(Config{ProviderName: "claude"}); cfg.APIKey != "a-key" {
			t.Fatalf("APIKey = %q, want the pre-existing ANTHROPIC fallback", cfg.APIKey)
		}
	})
}
