// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// The last item on the handoff's test-debt list: nothing asserted that a
// configured `ceiling_tokens` actually REACHES the ledger.
//
// Every piece existed and was tested on its own — config parsing, proxy.Ceilings,
// UsageActor.SetTenantCeilings, checkBudget — and the wire between them was one
// call in workspace.go that no test touched. A typo there would have left every
// tenant ceiling silently unenforced while `##proxy status` happily listed it.

// tenantConfig is a [proxy] tenants block with one capped customer.
func tenantConfig() config.ProxyTenantConfig {
	return config.ProxyTenantConfig{
		Tenants: map[string]config.TenantRule{
			"acme":   {Panes: []string{"pane-1"}, CeilingTokens: 1_000},
			"globex": {Panes: []string{"pane-2"}}, // no ceiling
		},
	}
}

func TestTenantCeiling_ConfigReachesTheLedgerAndBinds(t *testing.T) {
	// The exact call workspace.go makes when it spawns the usage actor.
	ceilings := proxy.Ceilings(tenantConfig())
	if ceilings["acme"] != 1_000 {
		t.Fatalf("proxy.Ceilings = %v, want acme:1000", ceilings)
	}
	if _, ok := ceilings["globex"]; ok {
		t.Error("a tenant with no ceiling must not be seeded with one")
	}

	u := NewUsageActor("s", nil, nil)
	u.SetTenantCeilings(ceilings)

	now := time.Now()
	// Under the ceiling: allowed.
	u.ingest(&msg.MsgUsageRecord{
		PaneID: "pane-1", Tenant: "acme", InTokens: 400, OutTokens: 100, TS: now})
	if reply := u.checkBudget("pane-1", "acme"); !reply.Ok {
		t.Fatalf("refused under the ceiling: %+v", reply)
	}

	// Over it: refused, and the refusal names the TENANT scope so the 429 tells
	// the operator which budget bound and which command changes it.
	u.ingest(&msg.MsgUsageRecord{
		PaneID: "pane-1", Tenant: "acme", InTokens: 400, OutTokens: 200, TS: now})
	reply := u.checkBudget("pane-1", "acme")
	if reply.Ok {
		t.Fatalf("a tenant over its configured ceiling was allowed: %+v", reply)
	}
	if reply.Scope != msg.UsageScopeTenant || reply.Tenant != "acme" {
		t.Fatalf("refusal scope = %q/%q, want tenant/acme", reply.Scope, reply.Tenant)
	}
	if reply.CeilingTokens != 1_000 {
		t.Fatalf("ceiling in the reply = %d, want the configured 1000", reply.CeilingTokens)
	}

	// A pane under a DIFFERENT tenant is unaffected — the cap is per customer,
	// not a session-wide brake.
	if r := u.checkBudget("pane-2", "globex"); !r.Ok {
		t.Fatalf("an uncapped tenant was refused: %+v", r)
	}
}

// TestTenantCeiling_PolicyKeysAlsoReachTheLedger covers the second source:
// policy's opaque budget map with "tenant:<name>" keys (022 §4.3), which is how
// an org file caps a customer without a policy schema change.
func TestTenantCeiling_PolicyKeysAlsoReachTheLedger(t *testing.T) {
	fromPolicy := tenantCeilingsFromPolicy(map[string]int64{
		"tenant:acme": 500,
		"pane-7":      9_999, // a pane ceiling, not a tenant one
		"tenant:":     123,   // malformed; must not create a tenant named ""
	})
	if fromPolicy["acme"] != 500 {
		t.Fatalf("policy tenant ceilings = %v", fromPolicy)
	}
	if _, ok := fromPolicy["pane-7"]; ok {
		t.Error("a pane ceiling leaked into the tenant map")
	}
	if _, ok := fromPolicy[""]; ok {
		t.Error("a malformed tenant: key created an empty-named tenant")
	}

	u := NewUsageActor("s", nil, nil)
	u.SetTenantCeilings(fromPolicy)
	u.ingest(&msg.MsgUsageRecord{
		PaneID: "p", Tenant: "acme", InTokens: 600, OutTokens: 0, TS: time.Now()})
	if reply := u.checkBudget("p", "acme"); reply.Ok {
		t.Fatalf("a policy-configured tenant ceiling did not bind: %+v", reply)
	}
}

// TestTenantCeiling_LowerOfTwoSourcesWins: config and policy both seed the same
// map, and policy's Merge already applies lower-wins ACROSS files. Within the
// ledger the first non-zero value must not be silently kept when a stricter one
// arrives — otherwise the order of two setter calls in workspace.go would decide
// a customer's budget.
func TestTenantCeiling_StricterSourceIsNotLost(t *testing.T) {
	// BOTH orders, because the point is that the answer does not depend on the
	// order the two setters happen to be called in.
	for _, order := range [][2]int64{{1_000, 200}, {200, 1_000}} {
		u := NewUsageActor("s", nil, nil)
		u.SetTenantCeilings(map[string]int64{"acme": order[0]})
		u.SetTenantCeilings(map[string]int64{"acme": order[1]})

		u.ingest(&msg.MsgUsageRecord{
			PaneID: "p", Tenant: "acme", InTokens: 300, OutTokens: 0, TS: time.Now()})
		reply := u.checkBudget("p", "acme")
		if reply.Ok {
			t.Fatalf("seeded %v: 300 tokens against a 200 ceiling was allowed: %+v", order, reply)
		}
		if reply.CeilingTokens != 200 {
			t.Fatalf("seeded %v: effective ceiling = %d, want the stricter 200",
				order, reply.CeilingTokens)
		}
	}
}
