// SPDX-License-Identifier: Apache-2.0

package meetingexec

// Audit integrity for sub-agent (Depth > 0) tool calls (F-99, F-105, ORDER-54
// commit 4, root-0461). A sub-agent's tool steps reach toolActor only over the
// asynchronous step stream; its calls reach the ordered result path through
// the sub_agent result (rysh-shared SubAgentCalls). Whatever the timing, every
// call that ran — at every depth — has exactly one "started" and exactly one
// terminal entry. The relay seam is the one in audit_integrity_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// treeProvider scripts a sub-agent tree. Each orchestrator is recognised by
// its first user turn (a child's is exactly its sub_agent task; anything else
// is the top level) and answers its plan in order, one response per round,
// then a final text.
type treeProvider struct {
	mu    sync.Mutex
	plans map[string][]provider.ToolCallRequest // first user turn ("" = top) -> calls, one per round
}

func (*treeProvider) Name() string { return "meeting-tree-test" }
func (*treeProvider) Complete(context.Context, string) (string, error) {
	return "", errors.New("unexpected Complete")
}
func (p *treeProvider) CompleteWithTools(_ context.Context, h []provider.ConversationTurn, _ []provider.ToolSpec, _ string) (*provider.AgenticResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := ""
	for _, t := range h {
		if t.Role == "user" {
			if _, ok := p.plans[t.Content]; ok {
				key = t.Content
			}
			break
		}
	}
	round := 0
	for _, t := range h {
		if t.Role == "assistant" {
			round++
		}
	}
	if plan := p.plans[key]; round < len(plan) {
		return &provider.AgenticResponse{StopReason: provider.StopReasonToolUse, ToolCalls: []provider.ToolCallRequest{plan[round]}}, nil
	}
	text := "Deployed to staging."
	if key != "" {
		text = key + " done."
	}
	return &provider.AgenticResponse{StopReason: provider.StopReasonEndTurn, TextBlocks: []provider.TextBlock{{Text: text}}}, nil
}

func tcall(id, name string, in map[string]string) provider.ToolCallRequest {
	b, _ := json.Marshal(in)
	return provider.ToolCallRequest{ID: id, Name: name, Input: b}
}

// subAgentTree: the top level delegates to child L1. At depth 1, L1 runs two
// bash calls. At depth 2, L1 runs one bash call and delegates to L2, which
// runs one bash call. Want: the audit entries per tool.
func subAgentTree(depth int) (*treeProvider, map[string]int) {
	p := &treeProvider{plans: map[string][]provider.ToolCallRequest{
		"": {tcall("p1", "sub_agent", map[string]string{"task": "L1"})},
	}}
	if depth == 1 {
		p.plans["L1"] = []provider.ToolCallRequest{
			tcall("c1", "bash", map[string]string{"command": "child one"}),
			tcall("c2", "bash", map[string]string{"command": "child two"}),
		}
		return p, map[string]int{"sub_agent": 1, "bash": 2}
	}
	p.plans["L1"] = []provider.ToolCallRequest{
		tcall("c1", "bash", map[string]string{"command": "child one"}),
		tcall("c2", "sub_agent", map[string]string{"task": "L2"}),
	}
	p.plans["L2"] = []provider.ToolCallRequest{
		tcall("g1", "bash", map[string]string{"command": "grandchild one"}),
	}
	return p, map[string]int{"sub_agent": 2, "bash": 2}
}

// assertAuditTree: per tool, exactly want[tool] "started" and want[tool]
// terminal entries; per (tool, target), as many terminals as starts; nothing
// else in the log. A tool with want -1 may appear any number of times, paired.
func assertAuditTree(t *testing.T, tr *trail, want map[string]int, terminals ...string) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(terminals) == 0 {
		terminals = []string{"ok"}
	}
	started, term := map[string]int{}, map[string]int{}
	pair := map[string]int{}
	var got []string
	for _, e := range tr.entries {
		got = append(got, e.Tool+":"+e.Status+":"+e.Target)
	}
	for _, e := range tr.entries {
		if _, ok := want[e.Tool]; !ok {
			t.Fatalf("unexpected tool %q in audit: %v", e.Tool, got)
		}
		key := e.Tool + "|" + e.Target
		switch e.Status {
		case "started":
			started[e.Tool]++
			pair[key]++
		case "ok", "error", "blocked_by_policy", "incomplete":
			ok := false
			for _, w := range terminals {
				ok = ok || e.Status == w
			}
			if !ok {
				t.Fatalf("terminal status %q not in %v: %v", e.Status, terminals, got)
			}
			term[e.Tool]++
			pair[key]--
		default:
			t.Fatalf("unknown audit status %q: %v", e.Status, got)
		}
	}
	for tool, n := range want {
		if n >= 0 && (started[tool] != n || term[tool] != n) {
			t.Fatalf("audit invariant broken for %s: want exactly %d started + %d terminal, got %d + %d: %v", tool, n, n, started[tool], term[tool], got)
		}
		if n < 0 && started[tool] != term[tool] {
			t.Fatalf("audit invariant broken for %s: %d started, %d terminal: %v", tool, started[tool], term[tool], got)
		}
	}
	for key, d := range pair {
		if d != 0 {
			t.Fatalf("started/terminal mismatch for %s (%+d): %v", key, d, got)
		}
	}
}

func childStep(minDepth int, kinds ...string) func(*msg.MsgAgenticStep) bool {
	return func(s *msg.MsgAgenticStep) bool {
		if s.Depth < minDepth {
			return false
		}
		if len(kinds) == 0 {
			return true
		}
		for _, k := range kinds {
			if s.Kind == k {
				return true
			}
		}
		return false
	}
}

// subAgentRun runs the depth-d tree with the steps hold matches held back
// until execute() has returned, and checks the invariant before and after
// they are released.
func subAgentRun(t *testing.T, depth int, hold func(*msg.MsgAgenticStep) bool) {
	t.Helper()
	p, want := subAgentTree(depth)
	tool := &gatedTool{}
	tr, r, out, err := relayRunWith(t, "", hold, nil, p, tool)
	if err != nil || out != "Deployed to staging." || int(tool.runs.Load()) != want["bash"] {
		t.Fatalf("out %q err %v bash runs %d", out, err, tool.runs.Load())
	}
	assertAuditTree(t, tr, want)
	if hold != nil {
		afterRelease(t, r)
		assertAuditTree(t, tr, want)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, e := range tr.entries {
		if e.Tool == "bash" && !strings.Contains(e.Target, " one") && !strings.Contains(e.Target, " two") {
			t.Fatalf("bash entry lost its target: %+v", e)
		}
	}
}

var depths = []int{1, 2}

// No step held: exact counts the moment execute() returns, at every depth.
// Run with -count=200, with and without -race.
func TestAuditSubAgentDirect(t *testing.T) {
	for _, d := range depths {
		t.Run(fmt.Sprintf("depth%d", d), func(t *testing.T) { subAgentRun(t, d, nil) })
	}
}

// The deepest child's StepToolEnd still in flight when the answer arrives.
func TestAuditSubAgentToolEndHeld(t *testing.T) {
	for _, d := range depths {
		t.Run(fmt.Sprintf("depth%d", d), func(t *testing.T) { subAgentRun(t, d, childStep(d, msg.StepToolEnd)) })
	}
}

// The deepest child's StepToolStart alone held.
func TestAuditSubAgentToolStartHeld(t *testing.T) {
	for _, d := range depths {
		t.Run(fmt.Sprintf("depth%d", d), func(t *testing.T) { subAgentRun(t, d, childStep(d, msg.StepToolStart)) })
	}
}

// EVERY sub-agent step (Depth >= 1) still in flight when the answer arrives:
// the step path has seen none of the child calls, so only the ordered source
// (the sub_agent result's SubAgentCalls) can audit them. RED without it.
func TestAuditSubAgentAllChildStepsInFlight(t *testing.T) {
	for _, d := range depths {
		t.Run(fmt.Sprintf("depth%d", d), func(t *testing.T) { subAgentRun(t, d, childStep(1)) })
	}
}

// Stopped while the deepest child is mid-tool: that call, started at depth d,
// gets exactly one terminal ("incomplete", or "error" if the interrupted
// result wins the race); every other entry stays paired.
func TestAuditSubAgentStoppedMidTool(t *testing.T) {
	for _, d := range depths {
		t.Run(fmt.Sprintf("depth%d", d), func(t *testing.T) {
			task := "L1"
			if d == 2 {
				task = "L2"
			}
			p := &treeProvider{plans: map[string][]provider.ToolCallRequest{
				"":   {tcall("p1", "sub_agent", map[string]string{"task": "L1"})},
				"L1": {tcall("c1", "sub_agent", map[string]string{"task": "L2"})},
			}}
			p.plans[task] = []provider.ToolCallRequest{tcall("x1", "bash", map[string]string{"command": "sleep 600"})}
			tool := &gatedTool{block: true, stopped: make(chan struct{})}
			startedBash := func(tr *trail) bool {
				for _, e := range tr.entries {
					if e.Tool == "bash" && e.Status == "started" {
						return true
					}
				}
				return false
			}
			tr, _, _, err := relayRunWith(t, "", nil, startedBash, p, tool)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("turn did not stop by cancel: %v", err)
			}
			assertAuditTree(t, tr, map[string]int{"bash": 1, "sub_agent": -1}, "incomplete", "error", "ok")
			tr.mu.Lock()
			defer tr.mu.Unlock()
			for _, e := range tr.entries {
				if e.Tool == "bash" && e.Status == "ok" {
					t.Fatalf("the stopped call was audited ok: %+v", e)
				}
			}
		})
	}
}
