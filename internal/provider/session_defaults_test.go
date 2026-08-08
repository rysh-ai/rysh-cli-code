package provider

import (
	"context"
	"testing"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"
)

// fakeOverridable records the model/effort of the last WithModelEffort call.
type fakeOverridable struct {
	lastModel, lastEffort string
	overridden            bool
}

func (f *fakeOverridable) Name() string                                     { return "fake" }
func (f *fakeOverridable) Complete(context.Context, string) (string, error) { return "ok", nil }
func (f *fakeOverridable) CompleteWithTools(
	context.Context, []sharedprovider.ConversationTurn, []sharedprovider.ToolSpec, string,
) (*sharedprovider.AgenticResponse, error) {
	return &sharedprovider.AgenticResponse{}, nil
}
func (f *fakeOverridable) WithModelEffort(model, effort string) sharedprovider.AgenticProvider {
	f.lastModel, f.lastEffort, f.overridden = model, effort, true
	return f
}

func TestSessionDefaults_UnsetPassesThrough(t *testing.T) {
	inner := &fakeOverridable{}
	p := WithSessionDefaults(inner, NewSessionDefaults())
	if _, err := p.CompleteWithTools(context.Background(), nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if inner.overridden {
		t.Errorf("no session default set, but WithModelEffort was called with %q/%q",
			inner.lastModel, inner.lastEffort)
	}
}

func TestSessionDefaults_AppliesPerCall(t *testing.T) {
	inner := &fakeOverridable{}
	d := NewSessionDefaults()
	p := WithSessionDefaults(inner, d)

	d.Set("claude-fable-5", "high")
	_, _ = p.CompleteWithTools(context.Background(), nil, nil, "")
	if inner.lastModel != "claude-fable-5" || inner.lastEffort != "high" {
		t.Errorf("session default not applied: %q/%q", inner.lastModel, inner.lastEffort)
	}

	// Clearing restores pass-through on the NEXT call (same wrapped provider).
	d.Set("", "")
	inner.overridden = false
	_, _ = p.CompleteWithTools(context.Background(), nil, nil, "")
	if inner.overridden {
		t.Errorf("cleared session default still overriding")
	}
}

// Explicit seats win over the session default, per-field: an explicit model
// with no effort inherits the session effort.
func TestSessionDefaults_ExplicitSeatWins(t *testing.T) {
	inner := &fakeOverridable{}
	d := NewSessionDefaults()
	d.Set("claude-fable-5", "high")
	p := WithSessionDefaults(inner, d).(sharedprovider.ModelEffortOverridable)

	p.WithModelEffort("claude-sonnet-5", "")
	if inner.lastModel != "claude-sonnet-5" || inner.lastEffort != "high" {
		t.Errorf("seat merge = %q/%q, want claude-sonnet-5/high", inner.lastModel, inner.lastEffort)
	}
}

func TestSessionDefaults_NonOverridableIdentity(t *testing.T) {
	// A provider without WithModelEffort is returned unchanged.
	type plain struct{ sharedprovider.AgenticProvider }
	inner := plain{}
	if got := WithSessionDefaults(inner, NewSessionDefaults()); got != sharedprovider.AgenticProvider(inner) {
		t.Errorf("expected identity for non-overridable provider")
	}
}
