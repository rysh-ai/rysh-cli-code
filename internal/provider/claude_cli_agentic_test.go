// SPDX-License-Identifier: Apache-2.0

package provider

// Tests for ClaudeCLI reachability (design 002 §3.2, roadmap B6). The CLI
// adapter existed but NO selection path reached it in agentic mode: every
// Claude-family name routed to the HTTP API, and the keyless fallback was the
// mock. These tests pin that "claude-cli" now selects it explicitly, needs no
// API key, and degrades tools honestly (Caps().Tools=false, text-only
// responses — never a fabricated tool call).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// interface conformance: the adapter must slot into the executor seam.
var _ AgenticProvider = (*ClaudeCLIAgentic)(nil)

// stubCLI writes a fake `claude` binary that records its argv to args.txt and
// prints a fixed answer, plus a system-prompt file for the Complete() path.
func stubCLI(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	argsFile := filepath.Join(dir, "args.txt")
	sh := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\necho cli-answer\n"
	if err := os.WriteFile(script, []byte(sh), 0o755); err != nil {
		t.Fatal(err)
	}
	promptFile := filepath.Join(dir, "system.md")
	if err := os.WriteFile(promptFile, []byte("file system prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		ProviderName:  "claude-cli",
		ClaudeCommand: script,
		SystemPrompt:  promptFile,
	}
}

func stubArgs(t *testing.T, cfg config.Config) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.ClaudeCommand), "args.txt"))
	if err != nil {
		t.Fatalf("stub args not recorded: %v", err)
	}
	return string(data)
}

// TestClaudeCLIReachableThroughSelection fails when the selection wiring is
// reverted: "claude-cli" then falls into the Claude default branch and the
// agentic provider names itself after the HTTP dialect.
func TestClaudeCLIReachableThroughSelection(t *testing.T) {
	cfg := stubCLI(t)

	ag := NewAgenticProvider(cfg)
	if _, ok := ag.(*ClaudeCLIAgentic); !ok {
		t.Fatalf("NewAgenticProvider(claude-cli) = %T, want *ClaudeCLIAgentic", ag)
	}
	if ag.Name() != "claude-cli" {
		t.Errorf("Name() = %q, want claude-cli", ag.Name())
	}

	// Explicit CLI selection wins even when an API key is configured — the
	// default branch's key-based auto-select must not apply.
	cfgWithKey := cfg
	cfgWithKey.APIKey = "sk-should-be-ignored"
	if _, ok := NewAgenticProvider(cfgWithKey).(*ClaudeCLIAgentic); !ok {
		t.Error("claude-cli selection must beat the api_key auto-select")
	}
	if _, ok := New(cfgWithKey).(*ClaudeCLIAgentic); !ok {
		t.Error("the simple selector must honor claude-cli too (lockstep rule)")
	}

	// Keyless gating: the CLI carries its own login, so has-key OR
	// needs-no-key must pass without any key — otherwise the agentic setup
	// demotes it to the mock provider.
	if RequiresAPIKey("claude-cli") {
		t.Error("claude-cli must not require an API key")
	}
	if !IsKnownProviderName("claude-cli") {
		t.Error("claude-cli must be a known provider name")
	}
}

// TestClaudeCLICapsAreHonest pins the design 002 §3.2 row: Complete-only,
// Tools=false — despite CompleteWithTools existing structurally, the Reporter
// declaration must win so tool-dependent consumers degrade loudly.
func TestClaudeCLICapsAreHonest(t *testing.T) {
	ag := NewClaudeCLIAgenticProvider(stubCLI(t))
	if SupportsTools(ag) {
		t.Fatal("claude-cli must report Tools=false (Complete-only adapter)")
	}
	c := Caps(ag)
	if c.Streaming || c.ParallelTools {
		t.Errorf("claude-cli must not claim streaming or parallel tools, got %+v", c)
	}
}

func TestClaudeCLICompleteWithToolsDegradesToText(t *testing.T) {
	cfg := stubCLI(t)
	ag := NewClaudeCLIAgenticProvider(cfg)

	resp, err := ag.CompleteWithTools(context.Background(),
		[]ConversationTurn{{Role: "user", Content: "what is here?"}},
		[]ToolSpec{{Name: "bash", Description: "ignored"}},
		"ASSEMBLED SYSTEM PROMPT",
	)
	if err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("a Complete-only adapter must never emit tool calls, got %+v", resp.ToolCalls)
	}
	if resp.StopReason != StopReasonEndTurn {
		t.Errorf("StopReason = %v, want end turn", resp.StopReason)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "cli-answer" {
		t.Errorf("TextBlocks = %+v, want the CLI's answer", resp.TextBlocks)
	}

	// The orchestrator's assembled system prompt must reach the CLI — not be
	// silently swapped for the static file.
	args := stubArgs(t, cfg)
	if !strings.Contains(args, "--system-prompt") || !strings.Contains(args, "ASSEMBLED SYSTEM PROMPT") {
		t.Errorf("CLI args missing the orchestrator system prompt:\n%s", args)
	}
	if !strings.Contains(args, "what is here?") {
		t.Errorf("CLI args missing the user prompt:\n%s", args)
	}
}

func TestFlattenConversation(t *testing.T) {
	// A single user turn passes through verbatim (no noisy role prefix).
	if got := flattenConversation([]ConversationTurn{{Role: "user", Content: "hi"}}); got != "hi" {
		t.Errorf("single-turn flatten = %q, want %q", got, "hi")
	}
	// Multi-turn keeps who-said-what.
	got := flattenConversation([]ConversationTurn{
		{Role: "user", Content: "run ls"},
		{Role: "assistant", Content: "ran it"},
		{Role: "tool", Content: "file.txt"},
	})
	for _, want := range []string{"User: run ls", "Assistant: ran it", "[tool result] file.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("flatten missing %q:\n%s", want, got)
		}
	}
}
