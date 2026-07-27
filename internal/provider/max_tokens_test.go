package provider

// Tests for the per-request MaxTokens seam through the rysh-cli decorators
// (design 002 follow-up, mirroring rysh-shared/provider/max_tokens_test.go
// and secretnat's wrapper contract): the cap must forward to the wrapped
// provider, the result must STAY the decorator (session default / pane
// override keep applying), a seam-less inner must surface nil so the
// ChatProvider path errors loudly, and end-to-end the cap must land on the
// wire.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"
)

// Interface guards (repo idiom, cf. claude_cli_agentic_test.go): both
// decorators must expose the per-request seams the executor and the
// ChatProvider path type-assert against.
var _ sharedprovider.MaxTokensOverridable = (*sessionAgenticProvider)(nil)
var _ sharedprovider.MaxTokensOverridable = (*paneAgenticProvider)(nil)
var _ sharedprovider.ModelEffortOverridable = (*sessionAgenticProvider)(nil)
var _ sharedprovider.ModelEffortOverridable = (*paneAgenticProvider)(nil)

// fakeCappableAgentic is a tool-capable provider with the MaxTokens seam:
// it records the last requested cap on the RECEIVER (spy) and carries the
// applied cap on the returned copy, so forwarding through decorators is
// observable.
type fakeCappableAgentic struct {
	fakeAgentic
	lastCap int
}

func (f *fakeCappableAgentic) WithMaxTokens(maxTokens int) AgenticProvider {
	f.lastCap = maxTokens
	cp := *f
	return &cp
}

// fakeSeamOverridable extends session_defaults_test.go's fakeOverridable
// with the MaxTokens seam, for the session decorator (whose constructor
// requires ModelEffortOverridable).
type fakeSeamOverridable struct {
	fakeOverridable
	lastCap int
}

func (f *fakeSeamOverridable) WithMaxTokens(maxTokens int) AgenticProvider {
	f.lastCap = maxTokens
	return f
}

var maxTokensChatTurns = []sharedprovider.Turn{
	{Role: sharedprovider.RoleUser, Blocks: []sharedprovider.Block{{Kind: sharedprovider.BlockKindText, Text: "hi"}}},
}

// TestSessionDefaults_ForwardsMaxTokens: (a) the cap reaches the inner spy,
// (b) the re-wrapped result is still the session decorator — a ##llm default
// installed AFTER capping applies on the next call.
func TestSessionDefaults_ForwardsMaxTokens(t *testing.T) {
	inner := &fakeSeamOverridable{}
	d := NewSessionDefaults()
	p := WithSessionDefaults(inner, d)

	mt, ok := p.(sharedprovider.MaxTokensOverridable)
	if !ok {
		t.Fatal("session decorator must expose the MaxTokens seam")
	}
	capped := mt.WithMaxTokens(700)
	if capped == nil {
		t.Fatal("WithMaxTokens over a cappable inner must not return nil")
	}
	if inner.lastCap != 700 {
		t.Errorf("cap did not reach the inner provider: lastCap=%d, want 700", inner.lastCap)
	}

	// Re-wrap contract: still the decorator, same defaults holder.
	if _, ok := capped.(*sessionAgenticProvider); !ok {
		t.Fatalf("capped result is %T, want *sessionAgenticProvider (re-wrap contract)", capped)
	}
	d.Set("claude-fable-5", "high")
	if _, err := capped.CompleteWithTools(context.Background(), nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if inner.lastModel != "claude-fable-5" || inner.lastEffort != "high" {
		t.Errorf("session default set after capping not applied: %q/%q", inner.lastModel, inner.lastEffort)
	}
}

// TestSessionDefaults_MaxTokensSeamlessInner: (c) a wrapped provider without
// the seam yields nil from WithMaxTokens, and a ChatRequest.MaxTokens through
// the decorator errors loudly instead of silently dropping the cap.
func TestSessionDefaults_MaxTokensSeamlessInner(t *testing.T) {
	// fakeOverridable has WithModelEffort (so the decorator engages) but no
	// MaxTokens seam.
	p := WithSessionDefaults(&fakeOverridable{}, NewSessionDefaults())
	mt, ok := p.(sharedprovider.MaxTokensOverridable)
	if !ok {
		t.Fatal("session decorator must expose the MaxTokens seam")
	}
	if got := mt.WithMaxTokens(5); got != nil {
		t.Fatalf("seam-less inner must yield nil, got %T", got)
	}
	_, err := sharedprovider.AsChatProvider(p).Chat(
		context.Background(),
		sharedprovider.ChatRequest{Turns: maxTokensChatTurns, MaxTokens: 5},
	)
	if err == nil {
		t.Fatal("ChatRequest.MaxTokens over a seam-less inner must error loudly")
	}
	if !strings.Contains(err.Error(), "MaxTokens") {
		t.Errorf("error should name the MaxTokens override, got: %v", err)
	}
}

// TestPaneOverride_ForwardsMaxTokens: (a) the cap reaches the effective
// provider, (b) the capped result is still the pane decorator — an override
// installed AFTER capping both routes the call and receives the cap.
func TestPaneOverride_ForwardsMaxTokens(t *testing.T) {
	holder := NewPaneOverride()
	base := &fakeCappableAgentic{fakeAgentic: fakeAgentic{name: "base"}}
	wrapped := WithPaneOverride(base, holder)

	mt, ok := wrapped.(sharedprovider.MaxTokensOverridable)
	if !ok {
		t.Fatal("pane decorator must expose the MaxTokens seam")
	}
	capped := mt.WithMaxTokens(700)
	if capped == nil {
		t.Fatal("WithMaxTokens over a cappable base must not return nil")
	}
	if _, ok := capped.(*paneAgenticProvider); !ok {
		t.Fatalf("capped result is %T, want *paneAgenticProvider (re-wrap contract)", capped)
	}

	if got := textOf(t, capped); got != "base" {
		t.Fatalf("capped call routed to %q, want base", got)
	}
	if base.lastCap != 700 {
		t.Errorf("cap did not reach the base provider: lastCap=%d, want 700", base.lastCap)
	}

	// Pane override still applies after WithMaxTokens: install one, the SAME
	// capped wrapper must route to it and forward the cap.
	ov := &fakeCappableAgentic{fakeAgentic: fakeAgentic{name: "override"}}
	holder.Set(ov)
	if got := textOf(t, capped); got != "override" {
		t.Fatalf("capped call after Set routed to %q, want override", got)
	}
	if ov.lastCap != 700 {
		t.Errorf("cap did not reach the override provider: lastCap=%d, want 700", ov.lastCap)
	}
}

// TestPaneOverride_MaxTokensSeamlessEffective: (c) nil when the effective
// provider lacks the seam + loud Chat error; and the race fallback — an
// override swapped to a seam-less provider AFTER capping fails the call
// instead of silently sending it uncapped.
func TestPaneOverride_MaxTokensSeamlessEffective(t *testing.T) {
	holder := NewPaneOverride()
	wrapped := WithPaneOverride(&fakeAgentic{name: "plain"}, holder)

	mt, ok := wrapped.(sharedprovider.MaxTokensOverridable)
	if !ok {
		t.Fatal("pane decorator must expose the MaxTokens seam")
	}
	if got := mt.WithMaxTokens(5); got != nil {
		t.Fatalf("seam-less effective provider must yield nil, got %T", got)
	}
	_, err := sharedprovider.AsChatProvider(wrapped).Chat(
		context.Background(),
		sharedprovider.ChatRequest{Turns: maxTokensChatTurns, MaxTokens: 5},
	)
	if err == nil {
		t.Fatal("ChatRequest.MaxTokens over a seam-less effective provider must error loudly")
	}
	if !strings.Contains(err.Error(), "MaxTokens") {
		t.Errorf("error should name the MaxTokens override, got: %v", err)
	}

	// Race fallback: cap validated against a cappable base, then the override
	// swaps to a seam-less provider — the call must fail loudly.
	holder2 := NewPaneOverride()
	base := &fakeCappableAgentic{fakeAgentic: fakeAgentic{name: "base"}}
	capped := WithPaneOverride(base, holder2).(sharedprovider.MaxTokensOverridable).WithMaxTokens(700)
	if capped == nil {
		t.Fatal("WithMaxTokens over a cappable base must not return nil")
	}
	holder2.Set(&fakeAgentic{name: "plain"})
	if _, err := capped.CompleteWithTools(context.Background(), nil, nil, ""); err == nil {
		t.Fatal("call after the override lost the seam must error, not go out uncapped")
	}
}

// TestDecorators_MaxTokensReachesWire: (d) end-to-end — ChatRequest.MaxTokens
// through the production stack pane(session(claude)) lands in the wire body's
// max_tokens field; without the override the construction-time default holds.
// Mirrors rysh-shared's body-capture pattern (provider/max_tokens_test.go).
func TestDecorators_MaxTokensReachesWire(t *testing.T) {
	const claudeJSONBody = `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, claudeJSONBody)
	}))
	defer srv.Close()

	claude := sharedprovider.NewClaudeAgenticProvider("k", srv.URL, "claude-x", 0)
	stacked := WithPaneOverride(WithSessionDefaults(claude, NewSessionDefaults()), NewPaneOverride())

	ctx := context.Background()
	if _, err := sharedprovider.AsChatProvider(stacked).Chat(ctx, sharedprovider.ChatRequest{Turns: maxTokensChatTurns, MaxTokens: 777}); err != nil {
		t.Fatalf("chat with MaxTokens through decorators: %v", err)
	}
	if _, err := sharedprovider.AsChatProvider(stacked).Chat(ctx, sharedprovider.ChatRequest{Turns: maxTokensChatTurns}); err != nil {
		t.Fatalf("chat without MaxTokens through decorators: %v", err)
	}

	wireMaxTokens := func(body []byte) int {
		var sent struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatalf("decode captured body: %v", err)
		}
		return sent.MaxTokens
	}
	if got := wireMaxTokens(bodies[0]); got != 777 {
		t.Errorf("wire max_tokens = %d, want 777", got)
	}
	if got, want := wireMaxTokens(bodies[1]), sharedprovider.DefaultMaxTokensForModel("claude-x"); got != want {
		t.Errorf("default wire max_tokens = %d, want %d", got, want)
	}
}
