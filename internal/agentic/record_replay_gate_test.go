package agentic

// Wiring tests for the record/replay provider seam (design 009 §3.2): the
// daemon builds its provider from the environment-carried record/replay
// directories, and replay counts as usable without any API key.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

func TestProviderUsable_ReplayNeedsNoKey(t *testing.T) {
	cfg := config.Config{ProviderName: "anthropic"} // keyless: normally unusable
	if providerUsable(cfg) {
		t.Fatal("precondition: keyless anthropic must be unusable")
	}
	t.Setenv(provider.ReplayDirEnv, t.TempDir())
	if !providerUsable(cfg) {
		t.Fatal("replay mode must be usable without a key — the transcript is the provider")
	}
}

func TestBuildAgenticProvider_ReplayEnvSelectsReplay(t *testing.T) {
	t.Setenv(provider.ReplayDirEnv, t.TempDir())
	p := buildAgenticProvider(config.Config{ProviderName: "anthropic"}, provider.NewSessionDefaults())
	if p.Name() != "replay" {
		t.Fatalf("provider under RYSH_REPLAY_DIR = %q, want replay", p.Name())
	}
}

func TestBuildAgenticProvider_RecordEnvWrapsLiveProvider(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(provider.RecordDirEnv, dir)
	// ollama: keyless-usable, so a live provider exists to wrap.
	p := buildAgenticProvider(config.Config{ProviderName: "ollama"}, provider.NewSessionDefaults())
	if p.Name() != "ollama" {
		t.Fatalf("recording must be name-transparent over the live provider, got %q", p.Name())
	}
	if _, err := os.Stat(filepath.Join(dir, provider.TranscriptFileName)); err != nil {
		t.Fatalf("recording wrapper must have prepared the transcript: %v", err)
	}
}

// A record dir that cannot be created must fail the provider closed — a
// green-but-unrecorded run would just move the failure to replay time.
func TestBuildAgenticProvider_UnwritableRecordDirFailsClosed(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A path UNDER a regular file can never be created.
	t.Setenv(provider.RecordDirEnv, filepath.Join(blocked, "rec"))
	p := buildAgenticProvider(config.Config{ProviderName: "ollama"}, provider.NewSessionDefaults())
	if p.Name() != "record-failed" {
		t.Fatalf("unpreparable record dir must fail the provider closed, got %q", p.Name())
	}
	if _, err := p.Complete(t.Context(), "x"); err == nil {
		t.Fatal("failed-closed provider must error every call")
	}
}

// Without record/replay env the selection is unchanged (mock fallback for
// keyless hosted providers, live otherwise).
func TestBuildAgenticProvider_NoEnvKeepsExistingSelection(t *testing.T) {
	p := buildAgenticProvider(config.Config{ProviderName: "anthropic"}, provider.NewSessionDefaults())
	if p.Name() != "mock-agentic" {
		t.Fatalf("keyless anthropic without replay env must fall back to mock, got %q", p.Name())
	}
	p = buildAgenticProvider(config.Config{ProviderName: "ollama"}, provider.NewSessionDefaults())
	if p.Name() != "ollama" {
		t.Fatalf("keyless ollama must stay live, got %q", p.Name())
	}
}
