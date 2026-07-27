package actors

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// These exercise the UsageActor's pure aggregation/budget logic directly,
// without NATS (kv nil ⇒ persistence no-ops).

func newTestUsageActor() *UsageActor { return NewUsageActor("test", nil, nil) }

func TestUsageActor_IngestAndSnapshot(t *testing.T) {
	u := newTestUsageActor()
	now := time.Now()
	u.ingest(&msg.MsgUsageRecord{PaneID: "pane-a", Model: "claude-opus-4-8", InTokens: 1000, OutTokens: 100, TS: now})
	u.ingest(&msg.MsgUsageRecord{PaneID: "pane-a", Model: "claude-opus-4-8", InTokens: 1000, OutTokens: 100, TS: now})
	u.ingest(&msg.MsgUsageRecord{PaneID: "pane-b", Model: "gpt-4o", InTokens: 1_000_000, OutTokens: 0, TS: now})

	snap := u.snapshot("today")
	if len(snap.ByPane) != 2 {
		t.Fatalf("ByPane len = %d, want 2", len(snap.ByPane))
	}
	// pane-b ($2.50) should sort before pane-a ($0.045) by cost desc.
	if snap.ByPane[0].Key != "pane-b" {
		t.Fatalf("top pane = %q, want pane-b (higher cost)", snap.ByPane[0].Key)
	}
	// pane-a: 2 calls, 2000 in, 200 out, cost 2*22500 = 45000.
	var a msg.UsageAgg
	for _, p := range snap.ByPane {
		if p.Key == "pane-a" {
			a = p
		}
	}
	if a.Calls != 2 || a.InTokens != 2000 || a.CostMicroUSD != 45000 {
		t.Fatalf("pane-a agg = %+v, want calls=2 in=2000 cost=45000", a)
	}
	// Session cost = 45000 + 2_500_000.
	if snap.SessionCostMicroUSD != 2_545_000 {
		t.Fatalf("session cost = %d, want 2545000", snap.SessionCostMicroUSD)
	}
}

func TestUsageActor_UnknownModelFlagged(t *testing.T) {
	u := newTestUsageActor()
	u.ingest(&msg.MsgUsageRecord{PaneID: "p", Model: "mystery-llm", InTokens: 500, OutTokens: 500, TS: time.Now()})
	snap := u.snapshot("today")
	if len(snap.ByPane) != 1 || !snap.ByPane[0].UnknownCost {
		t.Fatalf("expected UnknownCost flagged, got %+v", snap.ByPane)
	}
	if snap.ByPane[0].CostMicroUSD != 0 {
		t.Fatalf("unknown model cost = %d, want 0", snap.ByPane[0].CostMicroUSD)
	}
}

func TestUsageActor_BudgetCheck(t *testing.T) {
	u := newTestUsageActor()
	// No ceiling ⇒ always ok.
	if r := u.checkBudget("p"); !r.Ok || r.CeilingTokens != 0 {
		t.Fatalf("no ceiling: %+v, want Ok=true ceiling=0", r)
	}
	u.ingest(&msg.MsgUsageRecord{PaneID: "p", Model: "claude-opus-4-8", InTokens: 400, OutTokens: 200, TS: time.Now()})
	u.ceilings["p"] = 500 // spent tokens = 600 (400 in + 200 out)
	r := u.checkBudget("p")
	if r.Ok {
		t.Fatalf("over ceiling should be !Ok: %+v", r)
	}
	if r.SpentTokens != 600 || r.CeilingTokens != 500 {
		t.Fatalf("budget check = %+v, want spent=600 ceiling=500", r)
	}
}

func TestUsageActor_EmptySnapshot(t *testing.T) {
	u := newTestUsageActor()
	snap := u.snapshot("week")
	if len(snap.ByPane) != 0 || snap.SessionCostMicroUSD != 0 {
		t.Fatalf("empty snapshot not empty: %+v", snap)
	}
	if snap.Window != "week" {
		t.Fatalf("window = %q, want week", snap.Window)
	}
}
