package provider

// A cross-family ##llm switch has to change WHICH CLIENT the request goes to.
// These tests pin the decorator half of that: with a session provider
// installed, every call — one-shot, streaming, and the per-request override
// seams — must land on it rather than on the provider the config built.

import (
	"context"
	"testing"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"
)

// namedProvider is a fakeOverridable that reports a chosen name and counts the
// calls that reached it, so a test can tell the two providers apart.
type namedProvider struct {
	fakeOverridable
	name  string
	calls int
}

func (n *namedProvider) Name() string { return n.name }

func (n *namedProvider) CompleteWithTools(
	context.Context, []sharedprovider.ConversationTurn, []sharedprovider.ToolSpec, string,
) (*sharedprovider.AgenticResponse, error) {
	n.calls++
	return &sharedprovider.AgenticResponse{}, nil
}

func (n *namedProvider) WithModelEffort(model, effort string) sharedprovider.AgenticProvider {
	n.lastModel, n.lastEffort, n.overridden = model, effort, true
	return n
}

// TestSessionProvider_CallsReachTheSwitchedProvider is the whole point: after a
// cross-family switch the configured provider must see NO traffic.
func TestSessionProvider_CallsReachTheSwitchedProvider(t *testing.T) {
	configured := &namedProvider{name: "anthropic"}
	switched := &namedProvider{name: "openai"}

	defaults := NewSessionDefaults()
	p := WithSessionDefaults(configured, defaults)

	// Before the switch: traffic goes to the configured provider.
	if _, err := p.CompleteWithTools(context.Background(), nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if configured.calls != 1 || switched.calls != 0 {
		t.Fatalf("pre-switch calls: configured=%d switched=%d", configured.calls, switched.calls)
	}

	defaults.SetProvider("gpt-5.6-sol", "", switched)

	if _, err := p.CompleteWithTools(context.Background(), nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if switched.calls != 1 {
		t.Errorf("switched provider saw %d calls, want 1", switched.calls)
	}
	if configured.calls != 1 {
		t.Errorf("configured provider saw %d calls after the switch, want 1 (no new traffic)", configured.calls)
	}
	// The model must be applied to the SWITCHED provider, not the configured
	// one — applying it to the wrong side is the 404 this replaced.
	if switched.lastModel != "gpt-5.6-sol" {
		t.Errorf("switched provider model = %q, want gpt-5.6-sol", switched.lastModel)
	}
	if configured.overridden {
		t.Error("the configured provider was handed the other family's model id")
	}
}

// TestSessionProvider_NameAndUnwrapFollowTheSwitch: attribution and capability
// detection must describe the provider actually serving the session.
func TestSessionProvider_NameAndUnwrapFollowTheSwitch(t *testing.T) {
	configured := &namedProvider{name: "anthropic"}
	switched := &namedProvider{name: "openai"}
	defaults := NewSessionDefaults()
	p := WithSessionDefaults(configured, defaults)

	if p.Name() != "anthropic" {
		t.Errorf("pre-switch Name = %q", p.Name())
	}
	defaults.SetProvider("gpt-5.6-sol", "", switched)
	if p.Name() != "openai" {
		t.Errorf("post-switch Name = %q, want openai — usage attribution follows this", p.Name())
	}
	u, ok := p.(interface{ Unwrap() Provider })
	if !ok {
		t.Fatal("decorator no longer exposes Unwrap; Caps() would see the wrapper")
	}
	if u.Unwrap().Name() != "openai" {
		t.Errorf("Unwrap = %q, want the switched provider", u.Unwrap().Name())
	}
}

// TestSessionProvider_ClearRestoresConfigured: ##llm clear must retract the
// provider, not just the model.
func TestSessionProvider_ClearRestoresConfigured(t *testing.T) {
	configured := &namedProvider{name: "anthropic"}
	switched := &namedProvider{name: "openai"}
	defaults := NewSessionDefaults()
	p := WithSessionDefaults(configured, defaults)

	defaults.SetProvider("gpt-5.6-sol", "", switched)
	defaults.Set("", "")

	if defaults.Provider() != nil {
		t.Error("clear left a session provider installed")
	}
	if _, err := p.CompleteWithTools(context.Background(), nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if configured.calls != 1 {
		t.Errorf("after clear the configured provider saw %d calls, want 1", configured.calls)
	}
	if switched.calls != 0 {
		t.Errorf("after clear the switched provider still saw %d calls", switched.calls)
	}
}

// TestSessionProvider_ExplicitSeatLandsOnTheSwitchedProvider: a recipe/judge
// seat that pins only the model must still run on the switched family.
func TestSessionProvider_ExplicitSeatLandsOnTheSwitchedProvider(t *testing.T) {
	configured := &namedProvider{name: "anthropic"}
	switched := &namedProvider{name: "openai"}
	defaults := NewSessionDefaults()
	p := WithSessionDefaults(configured, defaults)
	defaults.SetProvider("gpt-5.6-sol", "medium", switched)

	mo, ok := p.(sharedprovider.ModelEffortOverridable)
	if !ok {
		t.Fatal("decorator lost the ModelEffortOverridable seam")
	}
	seat := mo.WithModelEffort("gpt-4o", "")
	if seat.Name() != "openai" {
		t.Errorf("explicit seat resolved to %q, want the switched provider", seat.Name())
	}
	if switched.lastModel != "gpt-4o" {
		t.Errorf("seat model = %q, want the explicit gpt-4o", switched.lastModel)
	}
	// The empty effort falls back to the session's.
	if switched.lastEffort != "medium" {
		t.Errorf("seat effort = %q, want the session's medium", switched.lastEffort)
	}
}
