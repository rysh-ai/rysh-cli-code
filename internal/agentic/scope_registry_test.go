package agentic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/tools"
)

// scopeStub is a no-op ToolExecutor for scope-registry tests.
type scopeStub struct{ name string }

func (s scopeStub) Execute(context.Context, json.RawMessage) (*tools.ToolOutput, error) {
	return &tools.ToolOutput{}, nil
}
func (s scopeStub) Spec() tools.ToolSpec                  { return tools.ToolSpec{Name: s.name} }
func (s scopeStub) RequiresApproval(json.RawMessage) bool { return false }

func has(reg *tools.ToolRegistry, name string) bool {
	_, ok := reg.Clone().Get(name) // Clone() flattens the whole chain, like a real run
	return ok
}

func TestParseScope(t *testing.T) {
	cases := map[string]struct {
		want ScopeKind
		ok   bool
	}{
		"":          {ScopeTab, true}, // default
		"tab":       {ScopeTab, true},
		"lane":      {ScopeLane, true},
		"panegroup": {ScopeGroup, true},
		"pane":      {ScopePane, true},
		"global":    {ScopeGlobal, false}, // not user-selectable
		"bogus":     {ScopeGlobal, false},
	}
	for in, exp := range cases {
		got, ok := ParseScope(in)
		if ok != exp.ok || (ok && got != exp.want) {
			t.Errorf("ParseScope(%q) = (%v,%v), want (%v,%v)", in, got, ok, exp.want, exp.ok)
		}
	}
}

func TestScopeVisibility(t *testing.T) {
	global := tools.NewToolRegistry()
	global.Register("g", scopeStub{name: "g"})
	svc := NewScopeRegistries(global)

	// Two panes in the same tab but different lanes.
	a := ScopeIDs{TabID: "t1", LaneID: "l1", GroupID: "gA", PaneID: "pA"}
	b := ScopeIDs{TabID: "t1", LaneID: "l2", GroupID: "gB", PaneID: "pB"}
	paneA := svc.PaneChain(a)
	paneB := svc.PaneChain(b)

	// Global tool is visible everywhere.
	if !has(paneA, "g") || !has(paneB, "g") {
		t.Fatalf("global tool not visible to both panes")
	}

	// Enable a tool at lane l1 → visible to pane A, not pane B.
	svc.RegistryFor(ScopeLane, a).Register("lane1tool", scopeStub{name: "lane1tool"})
	if !has(paneA, "lane1tool") {
		t.Fatalf("lane-scoped tool not visible to a pane in that lane")
	}
	if has(paneB, "lane1tool") {
		t.Fatalf("lane-scoped tool leaked to a pane in a different lane")
	}

	// Enable a tool at the tab → visible to both panes (same tab).
	svc.RegistryFor(ScopeTab, a).Register("tabtool", scopeStub{name: "tabtool"})
	if !has(paneA, "tabtool") || !has(paneB, "tabtool") {
		t.Fatalf("tab-scoped tool not visible to all panes in the tab")
	}

	// Enable a tool at pane A only → visible to A, not B.
	svc.RegistryFor(ScopePane, a).Register("panetool", scopeStub{name: "panetool"})
	if !has(paneA, "panetool") {
		t.Fatalf("pane-scoped tool not visible to its own pane")
	}
	if has(paneB, "panetool") {
		t.Fatalf("pane-scoped tool leaked to another pane")
	}
}

func TestScopeRegistryForIdempotent(t *testing.T) {
	svc := NewScopeRegistries(tools.NewToolRegistry())
	ids := ScopeIDs{TabID: "t", LaneID: "l", GroupID: "g", PaneID: "p"}
	first, second := svc.RegistryFor(ScopeLane, ids), svc.RegistryFor(ScopeLane, ids)
	if first != second {
		t.Fatalf("RegistryFor must return the same instance for the same scope id")
	}
	// A pane-scope enable must target the same instance PaneChain handed out.
	pane := svc.PaneChain(ids)
	if svc.RegistryFor(ScopePane, ids) != pane {
		t.Fatalf("RegistryFor(Pane) must return the live PaneChain instance")
	}
}

func TestScopeHintRoundTrip(t *testing.T) {
	ids := ScopeIDs{TabID: "t", LaneID: "l", GroupID: "g", PaneID: "p"}
	if got := ParseScopeHint(ids.Hint()); got != ids {
		t.Fatalf("hint round-trip: %+v != %+v", got, ids)
	}
	// Empty/partial hints are tolerated.
	if got := ParseScopeHint(""); got != (ScopeIDs{}) {
		t.Fatalf("empty hint should parse to zero ScopeIDs, got %+v", got)
	}
}

// TestGroupChainForAgentInheritance verifies an agent that inherits a pane's
// group chain sees the integration tools enabled at that pane's lane/global, but
// an agent inheriting a different lane's chain does not.
func TestGroupChainForAgentInheritance(t *testing.T) {
	global := tools.NewToolRegistry()
	global.Register("g", scopeStub{name: "g"})
	svc := NewScopeRegistries(global)

	a := ScopeIDs{TabID: "t1", LaneID: "l1", GroupID: "gA", PaneID: "pA"}
	b := ScopeIDs{TabID: "t1", LaneID: "l2", GroupID: "gB", PaneID: "pB"}

	// A lane-scoped tool enabled at lane l1 (as forge would).
	svc.RegistryFor(ScopeLane, a).Register("lanetool", scopeStub{name: "lanetool"})

	// Agent invoked from pane A inherits A's group chain → sees global + lane + own.
	agentA := tools.NewChildRegistry(svc.GroupChainFor(a))
	agentA.Register("agent_tool", scopeStub{name: "agent_tool"})
	for _, want := range []string{"g", "lanetool", "agent_tool"} {
		if _, ok := agentA.Clone().Get(want); !ok {
			t.Fatalf("agent inheriting lane l1 missing %q", want)
		}
	}

	// Agent invoked from pane B (different lane) does NOT see lane l1's tool.
	agentB := tools.NewChildRegistry(svc.GroupChainFor(b))
	if _, ok := agentB.Clone().Get("lanetool"); ok {
		t.Fatalf("agent inheriting lane l2 should not see lane l1's tool")
	}
	if _, ok := agentB.Clone().Get("g"); !ok {
		t.Fatalf("agent should still see global tools")
	}
}

func TestScopeCloseForgets(t *testing.T) {
	svc := NewScopeRegistries(tools.NewToolRegistry())
	ids := ScopeIDs{TabID: "t", PaneID: "p"}

	svc.RegistryFor(ScopePane, ids).Register("x", scopeStub{name: "x"})
	if !has(svc.RegistryFor(ScopePane, ids), "x") {
		t.Fatalf("pane tool should be present before close")
	}

	svc.Close(ScopePane, "p")
	// A fresh pane registry for the same id starts clean (the old instance was
	// forgotten); the leaf pane has no children, so there is no stale link.
	if has(svc.PaneChain(ids), "x") {
		t.Fatalf("pane registry should be empty after Close + recreate")
	}
}
