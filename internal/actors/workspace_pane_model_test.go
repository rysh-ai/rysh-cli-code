package actors

// Command-level tests for `##pane model`, in the workspace_pane_provider_test.go
// style: drive the extracted handler with a strings.Builder on a minimal
// WorkspaceActor. cmdPaneModel returns the message to publish (the caller owns
// the send), so no NATS is needed; the registry is a real one under t.TempDir().

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func paneModelCmd(t *testing.T, w *WorkspaceActor, paneID string, args ...string) (string, *msg.MsgPaneSetProvider) {
	t.Helper()
	var out strings.Builder
	m := w.cmdPaneModel(&out, paneID, args)
	return out.String(), m
}

func newPaneModelWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	return &WorkspaceActor{cfg: config.Config{ProviderName: "claude", RyshDir: t.TempDir()}}
}

func TestCmdPaneModel_NoActivePane(t *testing.T) {
	out, m := paneModelCmd(t, newPaneModelWorkspace(t), "", "anthropic/fable5")
	if m != nil {
		t.Fatal("no active pane must not produce a message")
	}
	if !strings.Contains(out, "no active pane") {
		t.Errorf("output: %s", out)
	}
}

// TestCmdPaneModel_SetFromRegistry: a seeded registry ref resolves to the
// model's API id and its provider family, on the same override seam
// ##pane provider uses.
func TestCmdPaneModel_SetFromRegistry(t *testing.T) {
	out, m := paneModelCmd(t, newPaneModelWorkspace(t), "p1", "anthropic/fable5")
	if m == nil {
		t.Fatal("a seeded ref must produce a MsgPaneSetProvider")
	}
	if m.Provider != "anthropic" || m.Model != "claude-fable-5" {
		t.Fatalf("message = %s/%s, want anthropic/claude-fable-5", m.Provider, m.Model)
	}
	for _, want := range []string{"anthropic/fable5", "claude-fable-5", "next agentic prompt", "persisted"} {
		if !strings.Contains(out, want) {
			t.Errorf("ack missing %q:\n%s", want, out)
		}
	}
	// Same family as the session provider — the config credentials still apply,
	// so no missing-key warning may appear.
	if strings.Contains(out, "warning") {
		t.Errorf("same-family selection must not warn:\n%s", out)
	}
}

// TestCmdPaneModel_CrossFamilyWarnsOnMissingKey: unlike the ##llm session
// default (Anthropic-only), a pane binds a whole provider, so an OpenAI entry
// is selectable — but a missing key is called out now, not on the next prompt.
func TestCmdPaneModel_CrossFamilyWarnsOnMissingKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	out, m := paneModelCmd(t, newPaneModelWorkspace(t), "p1", "openai/gpt-4o")
	if m == nil {
		t.Fatal("a known cross-family provider must be selectable per pane")
	}
	if m.Provider != "openai" || m.Model != "gpt-4o" {
		t.Fatalf("message = %s/%s, want openai/gpt-4o", m.Provider, m.Model)
	}
	if !strings.Contains(out, "OPENAI_API_KEY") {
		t.Errorf("cross-family ack must warn about the missing key:\n%s", out)
	}
}

// TestCmdPaneModel_NoAdapterRejected: the registry seeds grok, which rysh has
// no adapter for. Selecting it must be refused with the valid list rather than
// silently falling through to the Claude default branch.
func TestCmdPaneModel_NoAdapterRejected(t *testing.T) {
	out, m := paneModelCmd(t, newPaneModelWorkspace(t), "p1", "grok/grok-4")
	if m != nil {
		t.Fatal("a provider with no adapter must not produce a message")
	}
	if !strings.Contains(out, `provider "grok" has no rysh adapter`) {
		t.Errorf("output missing the rejection: %s", out)
	}
	if !strings.Contains(out, "anthropic") || !strings.Contains(out, "openai") {
		t.Errorf("rejection must list valid providers:\n%s", out)
	}
}

// TestCmdPaneModel_DisabledRefused: ##llm disable governs the pane door too.
func TestCmdPaneModel_DisabledRefused(t *testing.T) {
	w := newPaneModelWorkspace(t)
	if out := llmCmd(t, w, "disable", "anthropic/fable5"); !strings.Contains(out, "disabled") {
		t.Fatalf("disable setup: %s", out)
	}
	out, m := paneModelCmd(t, w, "p1", "anthropic/fable5")
	if m != nil {
		t.Fatal("a disabled model must not produce a message")
	}
	if !strings.Contains(out, "##llm enable anthropic/fable5") {
		t.Errorf("refusal must name the way back:\n%s", out)
	}
}

func TestCmdPaneModel_UnknownRefPointsAtAdd(t *testing.T) {
	out, m := paneModelCmd(t, newPaneModelWorkspace(t), "p1", "anthropic/nope")
	if m != nil {
		t.Fatal("unknown ref must not produce a message")
	}
	if !strings.Contains(out, "##llm add anthropic/nope") {
		t.Errorf("output: %s", out)
	}

	// A bare word (no slash) is a usage error, not a lookup.
	out, m = paneModelCmd(t, newPaneModelWorkspace(t), "p1", "fable5")
	if m != nil {
		t.Fatal("a ref without a provider must not produce a message")
	}
	if !strings.Contains(out, "usage: ##pane model <provider>/<name>") {
		t.Errorf("output: %s", out)
	}
}

func TestCmdPaneModel_DefaultClears(t *testing.T) {
	out, m := paneModelCmd(t, newPaneModelWorkspace(t), "p1", "default")
	if m == nil {
		t.Fatal("clear must produce a message")
	}
	if m.Provider != "" || m.Model != "" {
		t.Fatalf("clear message = %s/%s, want empty", m.Provider, m.Model)
	}
	// Provider and model share one override slot; the ack must not pretend
	// otherwise.
	if !strings.Contains(out, "provider+model override cleared") {
		t.Errorf("clear ack:\n%s", out)
	}
}

// TestCmdPaneModel_ListDelegatesToRegistry keeps `##pane model list` honest:
// it renders the same catalogue ##llm list does.
func TestCmdPaneModel_ListDelegatesToRegistry(t *testing.T) {
	out, m := paneModelCmd(t, newPaneModelWorkspace(t), "p1", "list")
	if m != nil {
		t.Fatal("list must not produce a message")
	}
	for _, want := range []string{"anthropic", "fable5", "openai"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q:\n%s", want, out)
		}
	}
}

func TestPaneModelStatusFormatting(t *testing.T) {
	// Override set: effective/provider/session all visible, with the way back.
	snap := &domain.PaneSnapshot{ProviderName: "openai", ProviderOverrideModel: "gpt-4o"}
	got := paneModelStatus(snap, "claude-opus-4-8 (built-in)")
	for _, want := range []string{"effective : gpt-4o", "provider  : openai", "##pane model default"} {
		if !strings.Contains(got, want) {
			t.Errorf("status missing %q:\n%s", want, got)
		}
	}

	// No override: falls back to the session model and points at the setter.
	got = paneModelStatus(&domain.PaneSnapshot{ProviderName: "claude"}, "claude-opus-4-8 (built-in)")
	if !strings.Contains(got, "override  : none") || !strings.Contains(got, "##pane model <provider>/<name>") {
		t.Errorf("no-override status:\n%s", got)
	}

	// No snapshot: still answers with the session model rather than erroring.
	if got = paneModelStatus(nil, "claude-opus-4-8 (built-in)"); !strings.Contains(got, "pane state unavailable") {
		t.Errorf("no-snapshot status:\n%s", got)
	}
}

// TestSessionModelLabel: the pane's fallback names the ##llm session default
// when one is active, and the config/built-in default otherwise.
func TestSessionModelLabel(t *testing.T) {
	w := newPaneModelWorkspace(t)
	if got := w.sessionModelLabel(); !strings.Contains(got, "built-in") {
		t.Errorf("no-override label = %q", got)
	}
	w.sessionLLMRef = "anthropic/fable5"
	if got := w.sessionModelLabel(); !strings.Contains(got, "anthropic/fable5") {
		t.Errorf("override label = %q", got)
	}
}
