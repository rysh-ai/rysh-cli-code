package actors

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestUsageActor_ByAgentRollup proves the ledger answers "what did my agents
// spend" (design 003 `##cost week` by agent): records carrying an AgentName are
// rolled up under that name, pane-only records are not, and the session total
// is NOT double-counted across the pane and agent views.
//
// In-memory: ingest/snapshot touch neither pub, nc, nor KV, so no broker.
func TestUsageActor_ByAgentRollup(t *testing.T) {
	u := NewUsageActor("t", nil, nil)
	now := time.Now().UTC()

	// A named agent spends over two calls (paneID == agent name for agents).
	u.ingest(&msg.MsgUsageRecord{PaneID: "code-reviewer", AgentName: "code-reviewer", InTokens: 100, OutTokens: 50, TS: now})
	u.ingest(&msg.MsgUsageRecord{PaneID: "code-reviewer", AgentName: "code-reviewer", InTokens: 20, OutTokens: 10, TS: now})
	// A regular pane spends with NO agent attribution.
	u.ingest(&msg.MsgUsageRecord{PaneID: "b1f9-uuid", AgentName: "", InTokens: 10, OutTokens: 5, TS: now})

	snap := u.snapshot("today")

	// --- by agent: exactly the named agent, both calls folded together ---
	if len(snap.ByAgent) != 1 {
		t.Fatalf("ByAgent len = %d, want 1 (%+v)", len(snap.ByAgent), snap.ByAgent)
	}
	a := snap.ByAgent[0]
	if a.Key != "code-reviewer" || a.InTokens != 120 || a.OutTokens != 60 || a.Calls != 2 {
		t.Fatalf("ByAgent[0] = %+v, want code-reviewer 120in/60out/2calls", a)
	}
	// The pane-only record must never appear in the agent view.
	for _, x := range snap.ByAgent {
		if x.Key == "b1f9-uuid" {
			t.Fatal("pane-only spend leaked into ByAgent")
		}
	}

	// --- by pane: both the agent (keyed by its name) and the pane ---
	panes := map[string]msg.UsageAgg{}
	for _, p := range snap.ByPane {
		panes[p.Key] = p
	}
	if _, ok := panes["code-reviewer"]; !ok {
		t.Fatalf("ByPane missing the agent's pane: %+v", snap.ByPane)
	}
	if _, ok := panes["b1f9-uuid"]; !ok {
		t.Fatalf("ByPane missing the regular pane: %+v", snap.ByPane)
	}

	// --- session total is the PANE sum only (no double count from ByAgent) ---
	// pane spend = (120+60) + (10+5) = 195; a double-count would report 375.
	if snap.SessionTokens != 195 {
		t.Fatalf("SessionTokens = %d, want 195 (double-count bug would give 375)", snap.SessionTokens)
	}
}
