package actors

// Command-level tests for `##pane provider` (design 002 §3.4, roadmap B6),
// in the workspace_workspace_cwd_test.go style: drive the extracted handler
// with a strings.Builder on a minimal WorkspaceActor. cmdPaneProvider returns
// the message to publish (the caller owns the send), so no NATS is needed.

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

func paneProviderCmd(t *testing.T, w *WorkspaceActor, paneID string, args ...string) (string, *msg.MsgPaneSetProvider) {
	t.Helper()
	var out strings.Builder
	m := w.cmdPaneProvider(&out, paneID, args)
	return out.String(), m
}

func newPaneProviderWorkspace() *WorkspaceActor {
	return &WorkspaceActor{cfg: config.Config{ProviderName: "claude"}}
}

func TestCmdPaneProvider_NoActivePane(t *testing.T) {
	out, m := paneProviderCmd(t, newPaneProviderWorkspace(), "", "gemini")
	if m != nil {
		t.Fatal("no active pane must not produce a message")
	}
	if !strings.Contains(out, "no active pane") {
		t.Errorf("output: %s", out)
	}
}

// TestCmdPaneProvider_UnknownListsValid: a typo must not silently pin the pane
// to the Claude default branch — the error names every valid provider.
func TestCmdPaneProvider_UnknownListsValid(t *testing.T) {
	out, m := paneProviderCmd(t, newPaneProviderWorkspace(), "p1", "got4")
	if m != nil {
		t.Fatal("unknown provider must not produce a message")
	}
	if !strings.Contains(out, `unknown provider "got4"`) {
		t.Errorf("output missing the rejection: %s", out)
	}
	for _, name := range provider.KnownProviderNames() {
		if !strings.Contains(out, name) {
			t.Errorf("error must list valid provider %q:\n%s", name, out)
		}
	}
}

func TestCmdPaneProvider_SetGeminiWithModel(t *testing.T) {
	// Blank the key env so the warning branch is deterministic.
	for _, v := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		t.Setenv(v, "")
	}
	out, m := paneProviderCmd(t, newPaneProviderWorkspace(), "p1", "gemini", "gemini-2.5-pro")
	if m == nil {
		t.Fatal("valid selection must produce a MsgPaneSetProvider")
	}
	if m.Provider != "gemini" || m.Model != "gemini-2.5-pro" {
		t.Fatalf("message = %s/%s, want gemini/gemini-2.5-pro", m.Provider, m.Model)
	}
	// Honest ack: applies on the next prompt (no respawn), persists, and the
	// missing key is called out now instead of failing later.
	for _, want := range []string{"next agentic prompt", "no respawn", "persisted", "GEMINI_API_KEY"} {
		if !strings.Contains(out, want) {
			t.Errorf("ack missing %q:\n%s", want, out)
		}
	}
}

// TestCmdPaneProvider_ClaudeCliNeedsNoKeyWarning: keyless gating is has-key OR
// needs-no-key — the CLI carries its own login, so no warning may appear.
func TestCmdPaneProvider_ClaudeCliNeedsNoKeyWarning(t *testing.T) {
	out, m := paneProviderCmd(t, newPaneProviderWorkspace(), "p1", "claude-cli")
	if m == nil {
		t.Fatal("claude-cli must be selectable")
	}
	if m.Provider != "claude-cli" {
		t.Fatalf("message provider = %q", m.Provider)
	}
	if strings.Contains(out, "warning") {
		t.Errorf("claude-cli needs no key; ack must not warn:\n%s", out)
	}
}

func TestCmdPaneProvider_DefaultClears(t *testing.T) {
	out, m := paneProviderCmd(t, newPaneProviderWorkspace(), "p1", "default")
	if m == nil {
		t.Fatal("clear must produce a message")
	}
	if m.Provider != "" || m.Model != "" {
		t.Fatalf("clear message = %s/%s, want empty", m.Provider, m.Model)
	}
	if !strings.Contains(out, "cleared") || !strings.Contains(out, "anthropic") {
		t.Errorf("clear ack should name the session provider family:\n%s", out)
	}
}

// TestCmdPaneProvider_ShowWithoutSnapshot: `##pane provider` with no args on a
// workspace that cannot resolve the pane snapshot still reports the session
// provider instead of erroring.
func TestCmdPaneProvider_ShowWithoutSnapshot(t *testing.T) {
	out, m := paneProviderCmd(t, newPaneProviderWorkspace(), "p1")
	if m != nil {
		t.Fatal("show must not produce a message")
	}
	if !strings.Contains(out, "anthropic") || !strings.Contains(out, "pane state unavailable") {
		t.Errorf("show output: %s", out)
	}
}

func TestPaneProviderStatusFormatting(t *testing.T) {
	// Override set: effective/override/session all visible, with the way back.
	snap := &domain.PaneSnapshot{
		ProviderName:          "gemini",
		ProviderOverride:      "gemini",
		ProviderOverrideModel: "gemini-2.5-pro",
	}
	got := paneProviderStatus(snap, "claude")
	for _, want := range []string{
		"effective : gemini",
		"override  : gemini (model gemini-2.5-pro)",
		"session   : anthropic",
		"##pane provider default",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing %q:\n%s", want, got)
		}
	}

	// No override: says so and points at the setter.
	got = paneProviderStatus(&domain.PaneSnapshot{ProviderName: "claude"}, "claude")
	if !strings.Contains(got, "override  : none") || !strings.Contains(got, "##pane provider <name>") {
		t.Errorf("no-override status:\n%s", got)
	}
}
