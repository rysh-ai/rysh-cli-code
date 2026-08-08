package actors

// Pane-side tests for the `##pane provider` override (design 002 §3.4,
// roadmap B6): applying MsgPaneSetProvider swaps the holder the executor's
// decorator consults per call, and the selection round-trips through the pane
// snapshot so it survives detach/attach.

import (
	"testing"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

func newProviderTestPane() *PaneActor {
	setup := &agentic.Setup{Provider: &sharedprovider.StaticAgenticProvider{}}
	return &PaneActor{
		id:             "p1",
		cfg:            config.Config{ProviderName: "claude"},
		agSetup:        setup,
		providerName:   setup.Provider.Name(),
		providerHolder: provider.NewPaneOverride(),
	}
}

func TestPaneSetProviderOverrideInstallsAndClears(t *testing.T) {
	p := newProviderTestPane()

	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{Provider: "gemini", Model: "gemini-2.5-pro"})
	if p.providerOverride != "gemini" || p.providerOverrideModel != "gemini-2.5-pro" {
		t.Fatalf("override fields = %q/%q", p.providerOverride, p.providerOverrideModel)
	}
	ov := p.providerHolder.Get()
	if ov == nil || ov.Name() != "gemini" {
		t.Fatalf("holder = %v, want a live gemini provider", ov)
	}
	if p.providerName != "gemini" {
		t.Errorf("effective providerName = %q, want gemini (snapshot shows the truth)", p.providerName)
	}
	if !p.kvDirty {
		t.Error("override must mark the pane KV-dirty so it persists")
	}

	// Clear: back to the session provider.
	p.kvDirty = false
	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{})
	if p.providerOverride != "" || p.providerHolder.Get() != nil {
		t.Fatal("clear must empty the override and the holder")
	}
	if p.providerName != p.agSetup.Provider.Name() {
		t.Errorf("providerName = %q, want the session provider's name", p.providerName)
	}
	if !p.kvDirty {
		t.Error("clear must also persist")
	}
}

// TestPaneSetProviderOverrideRejectsUnknown guards the raw NATS path: a bad
// message must not install anything (the workspace command already validates).
func TestPaneSetProviderOverrideRejectsUnknown(t *testing.T) {
	p := newProviderTestPane()
	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{Provider: "got4"})
	if p.providerOverride != "" || p.providerHolder.Get() != nil || p.kvDirty {
		t.Fatal("unknown provider must be ignored entirely")
	}
}

// TestPaneProviderOverrideSurvivesSnapshotRoundtrip is the detach/attach
// contract: the name/model pair rides the pane snapshot, and the restored pane
// re-installs the live provider in Started via installProviderOverride.
func TestPaneProviderOverrideSurvivesSnapshotRoundtrip(t *testing.T) {
	p := newProviderTestPane()
	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{Provider: "claude-cli"})

	snap := p.buildSnapshot(false, false, true)
	if snap.ProviderOverride != "claude-cli" {
		t.Fatalf("snapshot ProviderOverride = %q", snap.ProviderOverride)
	}
	if snap.ProviderName != "claude-cli" {
		t.Fatalf("snapshot ProviderName (effective) = %q", snap.ProviderName)
	}

	restored := newProviderTestPane()
	restored.RestoreState(snap)
	if restored.providerOverride != "claude-cli" {
		t.Fatalf("restored override = %q", restored.providerOverride)
	}
	// Started runs applyEffectiveProvider for restored panes; simulate it.
	restored.applyEffectiveProvider()
	ov := restored.providerHolder.Get()
	if ov == nil || ov.Name() != "claude-cli" {
		t.Fatalf("restored holder = %v, want claude-cli", ov)
	}
	// Capability honesty must follow the restored override too.
	if provider.SupportsTools(ov) {
		t.Error("restored claude-cli provider must report Tools=false")
	}
}
