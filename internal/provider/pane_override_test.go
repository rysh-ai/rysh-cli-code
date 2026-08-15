// SPDX-License-Identifier: Apache-2.0

package provider

// Tests for the ##pane provider decorator (design 002 §3.4, roadmap B6). The
// property that matters: the wrapper resolves the holder PER CALL, so an
// override installed (or cleared) after the executor spawned governs the very
// next prompt — and the capability table always describes the provider that
// will actually serve it.

import (
	"context"
	"testing"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"
)

// fakeAgentic is a minimal tool-capable provider whose responses carry its
// name, so routing is observable.
type fakeAgentic struct{ name string }

func (f *fakeAgentic) Name() string { return f.name }
func (f *fakeAgentic) Complete(_ context.Context, _ string) (string, error) {
	return f.name, nil
}
func (f *fakeAgentic) CompleteWithTools(
	_ context.Context, _ []ConversationTurn, _ []ToolSpec, _ string,
) (*AgenticResponse, error) {
	return &AgenticResponse{
		TextBlocks: []TextBlock{{Text: f.name}},
		StopReason: StopReasonEndTurn,
	}, nil
}

// fakeNoToolAgentic declares Tools=false (the claude-cli shape).
type fakeNoToolAgentic struct{ fakeAgentic }

func (f *fakeNoToolAgentic) Caps() Capabilities { return Capabilities{} }

func textOf(t *testing.T, p AgenticProvider) string {
	t.Helper()
	resp, err := p.CompleteWithTools(context.Background(), nil, nil, "")
	if err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	return resp.TextBlocks[0].Text
}

func TestPaneOverrideRoutesPerCall(t *testing.T) {
	holder := NewPaneOverride()
	wrapped := WithPaneOverride(&fakeAgentic{name: "base"}, holder)

	// No override: base serves.
	if got := textOf(t, wrapped); got != "base" {
		t.Fatalf("without override, call routed to %q, want base", got)
	}
	if wrapped.Name() != "base" {
		t.Errorf("Name() = %q, want base", wrapped.Name())
	}

	// Install: the SAME wrapper (already inside a spawned executor) must
	// route the next call to the override — this is the no-respawn contract.
	holder.Set(&fakeAgentic{name: "override"})
	if got := textOf(t, wrapped); got != "override" {
		t.Fatalf("after Set, call routed to %q, want override", got)
	}
	if wrapped.Name() != "override" {
		t.Errorf("Name() = %q, want override", wrapped.Name())
	}

	// Clear: back to base.
	holder.Set(nil)
	if got := textOf(t, wrapped); got != "base" {
		t.Fatalf("after clear, call routed to %q, want base", got)
	}
}

// TestPaneOverrideCapsFollowEffective: a claude-cli override must read
// Tools=false through the wrapper even while the base supports tools —
// otherwise design 006 §4.4 consumers would trust tools that cannot fire.
func TestPaneOverrideCapsFollowEffective(t *testing.T) {
	holder := NewPaneOverride()
	wrapped := WithPaneOverride(&fakeAgentic{name: "base"}, holder)

	if !SupportsTools(wrapped) {
		t.Fatal("base is tool-capable; wrapper must report it")
	}
	holder.Set(&fakeNoToolAgentic{fakeAgentic{name: "cli"}})
	if SupportsTools(wrapped) {
		t.Fatal("with a no-tool override installed, wrapper must report Tools=false")
	}
	holder.Set(nil)
	if !SupportsTools(wrapped) {
		t.Fatal("clearing the override must restore the base capabilities")
	}
}

func TestWithPaneOverrideNilHolderIsIdentity(t *testing.T) {
	base := &fakeAgentic{name: "base"}
	if got := WithPaneOverride(base, nil); got != AgenticProvider(base) {
		t.Fatalf("nil holder must return the base provider unchanged, got %T", got)
	}
}

// TestPaneOverrideModelEffortSeat: explicit seats resolve against whatever is
// effective at seat time; a non-overridable provider ignores the seat instead
// of erroring.
func TestPaneOverrideModelEffortSeat(t *testing.T) {
	holder := NewPaneOverride()
	wrapped := WithPaneOverride(&fakeAgentic{name: "base"}, holder)
	mo, ok := wrapped.(sharedprovider.ModelEffortOverridable)
	if !ok {
		t.Fatal("wrapper must satisfy ModelEffortOverridable for the executor's seat seam")
	}
	// fakeAgentic is not overridable: the seat degrades to the effective provider.
	if got := mo.WithModelEffort("some-model", "high"); got.Name() != "base" {
		t.Fatalf("seat on non-overridable provider = %q, want base passthrough", got.Name())
	}
}
