// SPDX-License-Identifier: Apache-2.0

package meetingexec

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// The Depth > 0 reconcile itself, without timing. Two sub_agent calls whose
// children REUSE the same call ids ("x", "y": ids are per provider
// conversation) are kept apart under their parent call; a depth-2 child is
// reached through its parent; a child start the step path saw with no
// terminal and no result is closed "incomplete"; an interrupted child call
// that never started writes nothing; a late child step after the reconcile
// writes nothing.
func TestReconcileSubAgentNamespacedExactCounts(t *testing.T) {
	tr := &trail{}
	ta := &toolActor{ctx: tr.ctx(context.Background())}
	ta.ledger.init()
	// Step path so far: child A (orch-a, depth 1) started+ended its x; child B
	// (orch-b) started its x only; grandchild (orch-g, depth 2) started z.
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolStart, Depth: 1, OrchestratorID: "orch-a", Origin: "bash", Title: "bash: a-x"})
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolEnd, Depth: 1, OrchestratorID: "orch-a", Origin: "bash", Title: "bash: a-x — ✓ ok"})
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolStart, Depth: 1, OrchestratorID: "orch-b", Origin: "bash", Title: "bash: b-x"})
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolStart, Depth: 2, OrchestratorID: "orch-g", Origin: "bash", Title: "bash: g-z"})
	conv := []provider.ConversationTurn{
		{Role: "assistant", ToolCalls: []provider.ToolCallRequest{{ID: "p1", Name: "sub_agent"}, {ID: "p2", Name: "sub_agent"}}},
		{Role: "tool", ToolCallID: "p1", Origin: "orch-a", Summary: "task a — ✓", SubAgentCalls: []provider.SubAgentCall{
			{ID: "x", Name: "bash", Title: "bash: a-x", Ended: true},
			{ID: "y", Name: "bash", Title: "bash: a-y", Ended: true, IsError: true}, // its steps never arrived
		}},
		{Role: "tool", ToolCallID: "p2", Origin: "orch-b", Summary: "task b — ✓", SubAgentCalls: []provider.SubAgentCall{
			{ID: "x", Name: "bash", Title: "bash: b-x", Ended: true},
			{ID: "y", Name: "sub_agent", Title: "sub_agent: inner", Ended: true, OrchestratorID: "orch-g", Calls: []provider.SubAgentCall{
				{ID: "z", Name: "bash", Title: "bash: g-z"},                                                // stopped mid-tool: no result
				{ID: "w", Name: "bash", Title: "bash: g-w", Ended: true, IsError: true, Interrupted: true}, // never ran
			}},
		}},
	}
	ta.reconcile(conv)
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolEnd, Depth: 2, OrchestratorID: "orch-g", Origin: "bash", Title: "bash: g-z — ✓ ok"})
	ta.finalise()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var got []string
	for _, e := range tr.entries {
		got = append(got, e.Tool+":"+e.Status+":"+e.Target)
	}
	sort.Strings(got)
	want := []string{
		"bash:error:a-y", "bash:incomplete:g-z", "bash:ok:a-x", "bash:ok:b-x",
		"bash:started:a-x", "bash:started:a-y", "bash:started:b-x", "bash:started:g-z",
		"sub_agent:ok:inner", "sub_agent:ok:task a", "sub_agent:ok:task b",
		"sub_agent:started:inner", "sub_agent:started:task a", "sub_agent:started:task b",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reconcile entries\n got %v\nwant %v", got, want)
	}
}

// Stopped without a result: finalise closes open starts at every depth.
func TestFinaliseClosesChildStarts(t *testing.T) {
	tr := &trail{}
	ta := &toolActor{ctx: tr.ctx(context.Background())}
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolStart, Depth: 1, OrchestratorID: "orch-a", Origin: "bash", Title: "bash: one"})
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolStart, Depth: 2, OrchestratorID: "orch-g", Origin: "bash", Title: "bash: two"})
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolEnd, Depth: 2, OrchestratorID: "orch-g", Origin: "bash", Title: "bash: two — ✓ ok"})
	ta.finalise()
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolStart, Depth: 1, OrchestratorID: "orch-a", Origin: "bash", Title: "bash: late"})
	assertAuditTree(t, tr, map[string]int{"bash": 2}, "ok", "incomplete")
}
