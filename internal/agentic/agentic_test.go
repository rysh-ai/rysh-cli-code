// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"os"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// ---------------------------------------------------------------------------
// Setup tests
// ---------------------------------------------------------------------------

func TestNewSetup_DisabledReturnsNil(t *testing.T) {
	os.Setenv("RYSH_AGENTIC_ENABLED", "false")
	defer os.Unsetenv("RYSH_AGENTIC_ENABLED")

	cfg := config.Config{
		ProviderName: "claude",
		SystemPrompt: "",
	}
	setup := NewSetup(cfg, nil)
	if setup != nil {
		t.Fatal("expected nil setup when agentic is disabled")
	}
}

func TestNewSetup_DefaultConfigNoAPIKey(t *testing.T) {
	// Ensure agentic is enabled
	os.Setenv("RYSH_AGENTIC_ENABLED", "true")
	defer os.Unsetenv("RYSH_AGENTIC_ENABLED")

	cfg := config.Config{
		ProviderName: "claude",
		SystemPrompt: "",
	}
	setup := NewSetup(cfg, nil)
	if setup == nil {
		t.Fatal("expected non-nil setup when agentic is enabled")
	}
	if setup.Provider == nil {
		t.Fatal("expected non-nil provider")
	}
	if setup.Provider.Name() != "mock-agentic" {
		t.Fatalf("expected mock-agentic provider, got %q", setup.Provider.Name())
	}
	if setup.ToolRegistry == nil {
		t.Fatal("expected non-nil tool registry")
	}
	if setup.SystemPrompt == "" {
		t.Fatal("expected non-empty system prompt (default)")
	}
}

func TestNewSetup_ToolRegistryPopulated(t *testing.T) {
	os.Setenv("RYSH_AGENTIC_ENABLED", "true")
	defer os.Unsetenv("RYSH_AGENTIC_ENABLED")

	cfg := config.Config{
		ProviderName: "claude",
		SystemPrompt: "",
	}
	setup := NewSetup(cfg, nil)
	if setup == nil {
		t.Fatal("expected non-nil setup")
	}

	expectedTools := []string{"bash", "file_read", "edit", "file_write", "glob", "grep"}
	specs := setup.ToolRegistry.AllSpecs()
	if len(specs) < len(expectedTools) {
		t.Fatalf("expected at least %d tools, got %d", len(expectedTools), len(specs))
	}

	registered := make(map[string]bool)
	for _, spec := range specs {
		registered[spec.Name] = true
	}
	for _, name := range expectedTools {
		if !registered[name] {
			t.Errorf("expected tool %q to be registered", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Helper function tests
// ---------------------------------------------------------------------------

func TestTruncate(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 8, "hello..."},
		{"", 5, ""},
		{"abcdef", 6, "abcdef"},
		{"abcdefg", 6, "abc..."},
		{"abc", 3, "abc"},
		{"abcd", 3, "..."},
	}
	for _, tc := range tests {
		got := truncate(tc.input, tc.maxLen)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.maxLen, got, tc.want)
		}
	}
}

func TestIndentBlock(t *testing.T) {
	tests := []struct {
		text   string
		prefix string
		want   string
	}{
		{"hello", "  ", "  hello"},
		{"line1\nline2", ">> ", ">> line1\n>> line2"},
		{"line1\n\nline3", "  ", "  line1\n\n  line3"},
		{"", "  ", ""},
		{"single", "", "single"},
	}
	for _, tc := range tests {
		got := indentBlock(tc.text, tc.prefix)
		if got != tc.want {
			t.Errorf("indentBlock(%q, %q) = %q, want %q", tc.text, tc.prefix, got, tc.want)
		}
	}
}

func TestFormatTimestamp(t *testing.T) {
	ts := formatTimestamp()
	if ts == "" {
		t.Fatal("expected non-empty timestamp")
	}
	// Should be in HH:MM:SS format (8 chars)
	if len(ts) != 8 {
		t.Errorf("expected timestamp length 8 (HH:MM:SS), got %d: %q", len(ts), ts)
	}
}

// ---------------------------------------------------------------------------
// Static agentic provider tests
// ---------------------------------------------------------------------------

func TestStaticAgenticProvider_Complete(t *testing.T) {
	p := &staticAgenticProvider{name: "mock-agentic"}

	result, err := p.Complete(context.Background(), "test prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}
	expected := "Agentic mode requires an API key. Set RYSH_API_KEY or configure api_key in rysh.config.yaml."
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestStaticAgenticProvider_CompleteWithTools(t *testing.T) {
	p := &staticAgenticProvider{name: "mock-agentic"}

	resp, err := p.CompleteWithTools(
		context.Background(),
		[]provider.ConversationTurn{{Role: "user", Content: "hello"}},
		nil,
		"system prompt",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.StopReason != provider.StopReasonEndTurn {
		t.Errorf("expected stop reason %q, got %q", provider.StopReasonEndTurn, resp.StopReason)
	}
	if len(resp.TextBlocks) != 1 {
		t.Fatalf("expected 1 text block, got %d", len(resp.TextBlocks))
	}
	expected := "Agentic mode requires an API key. Set RYSH_API_KEY or configure api_key in rysh.config.yaml."
	if resp.TextBlocks[0].Text != expected {
		t.Errorf("got %q, want %q", resp.TextBlocks[0].Text, expected)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(resp.ToolCalls))
	}
}

func TestStaticAgenticProvider_Name(t *testing.T) {
	p := &staticAgenticProvider{name: "mock-agentic"}
	if p.Name() != "mock-agentic" {
		t.Errorf("expected name %q, got %q", "mock-agentic", p.Name())
	}
}
