// SPDX-License-Identifier: Apache-2.0

package board

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func depPane(id, label, work string, blockedBy ...string) FlowNode {
	return FlowNode{ID: id, Kind: FlowNodePane, Label: label, Work: work, BlockedBy: blockedBy}
}

func TestFlowDepsNobodyBlocked(t *testing.T) {
	d := BuildFlowDeps(Flow{Nodes: []FlowNode{depPane("a", "a", "progress")}})
	if d.Blocked != 0 || d.Bottleneck() != nil || RenderFlowDeps(d) != nil {
		t.Fatalf("deps with nobody blocked: %+v", d)
	}
}

// The bottleneck is the unblocked agent with the most agents waiting on it,
// directly or transitively; a chain behind it counts in full.
func TestFlowDepsBottleneckIsTheMostWaitedOn(t *testing.T) {
	f := Flow{Nodes: []FlowNode{
		{ID: "board:x", Kind: FlowNodeBoard, Label: "x"},
		depPane("mgr", "mgr", "progress"),
		depPane("w1", "w1", "blocked", "mgr"),
		depPane("w2", "w2", "blocked", "w1"),
		depPane("w3", "w3", "blocked", "mgr", "human"),
		depPane("w4", "w4", "progress"),
		depPane("w5", "w5", "blocked", "w4"),
	}}
	d := BuildFlowDeps(f)
	if d.Blocked != 4 {
		t.Fatalf("blocked = %d, want 4", d.Blocked)
	}
	var roots []string
	var waiting []int
	for _, r := range d.Roots {
		roots = append(roots, r.Label)
		waiting = append(waiting, r.Waiting)
	}
	// mgr: w1, w2, w3. w4: w5. human: w3.
	if !reflect.DeepEqual(roots, []string{"mgr", "w4", "human"}) || !reflect.DeepEqual(waiting, []int{3, 1, 1}) {
		t.Fatalf("roots %v waiting %v", roots, waiting)
	}
	if b := d.Bottleneck(); b == nil || b.Label != "mgr" {
		t.Fatalf("bottleneck = %+v, want mgr", b)
	}
	got := strings.Join(RenderFlowDeps(d), "\n")
	for _, want := range []string{
		"dependencies · 4 blocked · bottleneck: mgr (3 waiting)",
		"  ▣ mgr {progress}  ← 3 waiting",
		"    ├─ w1 {blocked}  ← 1 waiting",
		"    │  └─ w2 {blocked}",
		"    └─ w3 {blocked}  · also blocked by human",
		"  ▣ human  ← 1 waiting",
		"    └─ w3 {blocked}  · also blocked by mgr",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}

// A diamond — two paths to one agent under the same root — draws the agent's
// subtree once and marks the second sighting.
func TestFlowDepsDiamondDrawnOnce(t *testing.T) {
	d := BuildFlowDeps(Flow{Nodes: []FlowNode{
		depPane("r", "r", "progress"),
		depPane("a", "a", "blocked", "r"),
		depPane("b", "b", "blocked", "r"),
		depPane("c", "c", "blocked", "a", "b"),
		depPane("e", "e", "blocked", "c"),
	}})
	if d.Roots[0].Waiting != 4 {
		t.Fatalf("r waiting = %d, want 4 distinct (a, b, c, e)", d.Roots[0].Waiting)
	}
	got := strings.Join(RenderFlowDeps(d), "\n")
	if strings.Count(got, "e {blocked}") != 1 || !strings.Contains(got, "(see above)") {
		t.Errorf("diamond drawn twice or unmarked:\n%s", got)
	}
}

// A deadlock has no root; it is listed with the agents stuck behind it.
func TestFlowDepsDeadlockListedWithItsWaiters(t *testing.T) {
	d := BuildFlowDeps(Flow{Nodes: []FlowNode{
		depPane("a", "a", "blocked", "b"),
		depPane("b", "b", "blocked", "a"),
		depPane("c", "c", "blocked", "a"),
	}})
	if len(d.Roots) != 0 {
		t.Fatalf("a pure deadlock has no root: %+v", d.Roots)
	}
	if len(d.Cycles) != 1 || !reflect.DeepEqual(d.Cycles[0].Members, []string{"a", "b"}) || d.Cycles[0].Waiting != 1 {
		t.Fatalf("cycles = %+v", d.Cycles)
	}
	got := strings.Join(RenderFlowDeps(d), "\n")
	for _, want := range []string{"3 blocked · 1 deadlock", "⟳ DEADLOCK  a → b → a · 1 waiting on it"} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}

// Blockers that are not nodes on this board still root a tree, by name.
func TestFlowDepsOutsideBlockers(t *testing.T) {
	d := BuildFlowDeps(Flow{Nodes: []FlowNode{
		depPane("a", "a", "blocked", "ext:ci"),
		depPane("b", "b", "blocked", "0123456789abcdef"),
	}})
	var roots []string
	for _, r := range d.Roots {
		roots = append(roots, r.Label)
	}
	if !reflect.DeepEqual(roots, []string{"pane-01234567", "ext:ci"}) && !reflect.DeepEqual(roots, []string{"ext:ci", "pane-01234567"}) {
		t.Fatalf("roots = %v", roots)
	}
}

// The golden fixture's ledger: wk-a1 is blocked by mgr-a and the human.
func TestFlowDepsFromTheGoldenFixture(t *testing.T) {
	in := loadFlowFixture(t)
	d := BuildFlowDeps(BuildFlow(in.Board, in.Threads, in.Panes))
	got := strings.Join(RenderFlowDeps(d), "\n")
	for _, want := range []string{
		"dependencies · 1 blocked · bottleneck: mgr-a (1 waiting)",
		"  ▣ mgr-a {progress·working}  ← 1 waiting",
		"    └─ wk-a1 {blocked·idle}  · also blocked by human",
		"  ▣ human  ← 1 waiting",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}

type flowFixture struct {
	Board   string     `json:"board"`
	Threads []Thread   `json:"threads"`
	Panes   []FlowPane `json:"panes"`
}

func loadFlowFixture(t *testing.T) flowFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/flow-input.json")
	if err != nil {
		t.Fatal(err)
	}
	var in flowFixture
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

// Too tall to fit: every root and every deadlock survive, compacted, before
// anything is cut.
func TestFlowDepsFitKeepsRootsAndDeadlocks(t *testing.T) {
	d := BuildFlowDeps(Flow{Nodes: []FlowNode{
		depPane("api", "api", "progress"),
		depPane("ui", "ui", "blocked", "api"),
		depPane("e2e", "e2e", "blocked", "ui", "api"),
		depPane("rel", "rel", "blocked", "e2e", "docs", "cache"),
		depPane("docs", "docs", "blocked", "human"),
		depPane("cache", "cache", "blocked", "auth"),
		depPane("auth", "auth", "blocked", "cache"),
	}})
	if full := RenderFlowDeps(d); len(full) <= 5 {
		t.Fatalf("fixture too small to need fitting: %d rows", len(full))
	}
	got := RenderFlowDepsFit(d, 5)
	want := []string{
		"dependencies · 6 blocked · bottleneck: api (3 waiting) · 1 deadlock",
		"  ▣ api {progress}  ← 3 waiting  ⇐ ui, e2e",
		"  ▣ human  ← 2 waiting  ⇐ docs",
		"  ⟳ DEADLOCK  cache → auth → cache · 1 waiting on it",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fit:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if cut := RenderFlowDepsFit(d, 3); len(cut) != 3 || !strings.Contains(cut[2], "more") {
		t.Fatalf("cut: %q", cut)
	}
}
