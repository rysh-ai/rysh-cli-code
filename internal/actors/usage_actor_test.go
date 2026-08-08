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
	if r := u.checkBudget("p", ""); !r.Ok || r.CeilingTokens != 0 {
		t.Fatalf("no ceiling: %+v, want Ok=true ceiling=0", r)
	}
	u.ingest(&msg.MsgUsageRecord{PaneID: "p", Model: "claude-opus-4-8", InTokens: 400, OutTokens: 200, TS: time.Now()})
	u.ceilings["p"] = 500 // spent tokens = 600 (400 in + 200 out)
	r := u.checkBudget("p", "")
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

// ---------------------------------------------------------------------------
// Per-tenant accounting (design 022 §4.3)
// ---------------------------------------------------------------------------

// TestUsageActor_TenantIndexDoesNotDoubleCount is the regression the rejected
// design would have caused. Tenant accounting is a SECOND INDEX over the same
// records, not a second stream of them — emitting a synthetic `tenant:` record
// would have inflated every `##cost` figure by the tenanted portion.
func TestUsageActor_TenantIndexDoesNotDoubleCount(t *testing.T) {
	now := time.Now()
	rec := func(tenant string) *msg.MsgUsageRecord {
		return &msg.MsgUsageRecord{
			PaneID: "pane-a", Model: "claude-opus-4-8",
			InTokens: 1000, OutTokens: 100, Tenant: tenant, TS: now,
		}
	}

	plain := newTestUsageActor()
	plain.ingest(rec(""))
	plain.ingest(rec(""))
	want := plain.snapshot("today")

	tenanted := newTestUsageActor()
	tenanted.ingest(rec("acme"))
	tenanted.ingest(rec("acme"))
	got := tenanted.snapshot("today")

	if got.SessionCostMicroUSD != want.SessionCostMicroUSD {
		t.Fatalf("session cost changed once records carried a tenant: %d, want %d",
			got.SessionCostMicroUSD, want.SessionCostMicroUSD)
	}
	if got.SessionTokens != want.SessionTokens {
		t.Fatalf("session tokens changed once records carried a tenant: %d, want %d",
			got.SessionTokens, want.SessionTokens)
	}
	if len(got.ByPane) != len(want.ByPane) {
		t.Fatalf("ByPane length changed: %d, want %d", len(got.ByPane), len(want.ByPane))
	}
	// And the tenant index did accumulate — otherwise this test passes for the
	// wrong reason (nothing was indexed at all).
	if spent := tenanted.spentTodayTenant("acme"); spent != 2200 {
		t.Fatalf("tenant spend = %d, want 2200 (2 x 1100)", spent)
	}
}

// TestUsageActor_TenantCeilingBindsWithPaneHeadroom: a pane well under its own
// ceiling must still be refused when its customer is out of budget, or a tenant
// cap is escaped simply by opening another pane.
func TestUsageActor_TenantCeilingBindsWithPaneHeadroom(t *testing.T) {
	u := newTestUsageActor()
	u.SetTenantCeilings(map[string]int64{"acme": 1000})
	u.ceilings["pane-a"] = 1_000_000 // pane has plenty of room

	u.ingest(&msg.MsgUsageRecord{
		PaneID: "pane-a", Model: "claude-opus-4-8",
		InTokens: 900, OutTokens: 200, Tenant: "acme", TS: time.Now(),
	})

	r := u.checkBudget("pane-a", "acme")
	if r.Ok {
		t.Fatalf("tenant over ceiling must refuse even with pane headroom: %+v", r)
	}
	if r.Scope != msg.UsageScopeTenant || r.Tenant != "acme" {
		t.Fatalf("reply must name the binding scope: %+v", r)
	}
	if r.SpentTokens != 1100 || r.CeilingTokens != 1000 {
		t.Fatalf("reply carries the wrong figures: %+v", r)
	}

	// The same pane, unattributed, is still fine — the tenant ceiling binds the
	// tenant, not the pane.
	if r := u.checkBudget("pane-a", ""); !r.Ok {
		t.Fatalf("untenanted check should pass on pane headroom: %+v", r)
	}
}

// TestUsageActor_TenantWithoutCeilingIsUnbounded keeps the default honest: a
// tenant that exists for attribution only must never be refused.
func TestUsageActor_TenantWithoutCeilingIsUnbounded(t *testing.T) {
	u := newTestUsageActor()
	u.ingest(&msg.MsgUsageRecord{
		PaneID: "pane-a", InTokens: 10_000_000, Tenant: "acme", TS: time.Now(),
	})
	if r := u.checkBudget("pane-a", "acme"); !r.Ok {
		t.Fatalf("a tenant with no ceiling must not be refused: %+v", r)
	}
}
