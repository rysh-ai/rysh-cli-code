package config

// Tests for the Gemini key sourcing in applyEnvOverrides (design 002 §3.4,
// roadmap B6). The documented invocation is:
//
//	RYSH_PROVIDER=gemini GEMINI_API_KEY=... rysh
//
// with GOOGLE_API_KEY as the fallback variable. The dangerous cases are key
// CROSS-CONTAMINATION: an ambient ANTHROPIC_API_KEY must never be handed to
// Gemini (a foreign key just yields a confusing upstream 401), and RYSH_API_KEY
// stays the explicit override for every provider.

import "testing"

// clearProviderKeyEnv blanks every variable applyEnvOverrides consults for the
// provider/key pair, so ambient developer environments can't skew the test.
func clearProviderKeyEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"RYSH_PROVIDER", "RYSH_API_KEY",
		"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
	} {
		t.Setenv(v, "")
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
