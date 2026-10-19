// SPDX-License-Identifier: Apache-2.0

package board

// The dependency tree (design 054 §8a): who is waiting on whom, rooted at the
// bottlenecks.
//
// A flow's `blocks` edges say "A is blocked by B", one pair at a time. What a
// human watching a fleet wants is the other direction, and transitively: which
// agent is the reason the most others are stuck. So the tree is rooted at the
// blockers that are NOT themselves blocked — the ones that can actually move —
// with everything waiting on each underneath it, and the roots ranked by how
// many agents wait on them, directly or not. The first root is the bottleneck.
//
// Derived from Flow.Nodes' BlockedBy and nothing else: a pure function of the
// flow model, so it adds nothing to 052's JSON contract and the TUI and
// `rysh board tail --view flow` cannot disagree about it.

import (
	"sort"
	"strconv"
	"strings"
)

// FlowDepNode is one agent (or outside blocker) in the dependency tree.
type FlowDepNode struct {
	ID       string
	Label    string
	Work     string
	Activity string
	Health   string
	// Waiting is how many distinct agents wait on this one, directly or
	// transitively.
	Waiting int
	// Also are this agent's OTHER blockers — it is drawn once under each, and
	// a reader must know that unblocking one parent is not enough.
	Also []string
	// Repeat marks a node already drawn higher in the same root's tree (a
	// diamond): its subtree is not drawn twice.
	Repeat   bool
	Children []FlowDepNode
}

// FlowDepCycle is one deadlock.
type FlowDepCycle struct {
	Members []string // labels, in edge order: each is blocked by the next
	// Waiting is how many agents outside the ring wait on it.
	Waiting int
}

// FlowDeps is the whole dependency picture of a board.
type FlowDeps struct {
	// Roots are the blockers nobody blocks, most-waited-on first.
	Roots []FlowDepNode
	// Cycles are deadlocks: agents blocked on each other in a ring. No root
	// reaches them, so they are listed apart.
	Cycles []FlowDepCycle
	// Blocked is how many agents are blocked at all.
	Blocked int
}

// Bottleneck is the root most agents wait on, or nil when nobody is blocked.
func (d FlowDeps) Bottleneck() *FlowDepNode {
	if len(d.Roots) == 0 {
		return nil
	}
	return &d.Roots[0]
}

// BuildFlowDeps derives the dependency tree from a flow model.
func BuildFlowDeps(f Flow) FlowDeps {
	byID := map[string]*FlowNode{}
	order := map[string]int{}
	for i := range f.Nodes {
		byID[f.Nodes[i].ID] = &f.Nodes[i]
		order[f.Nodes[i].ID] = i
	}
	// blockedBy: agent → its blockers; waiters: blocker → agents blocked by it.
	blockedBy := map[string][]string{}
	waiters := map[string][]string{}
	for _, n := range f.Nodes {
		if n.Kind != FlowNodePane || len(n.BlockedBy) == 0 {
			continue
		}
		for _, b := range n.BlockedBy {
			if b == "" || b == n.ID {
				continue
			}
			blockedBy[n.ID] = append(blockedBy[n.ID], b)
			waiters[b] = append(waiters[b], n.ID)
		}
	}
	var deps FlowDeps
	deps.Blocked = len(blockedBy)
	if deps.Blocked == 0 {
		return deps
	}

	label := func(id string) string {
		if n := byID[id]; n != nil {
			return SanitiseInline(n.Label)
		}
		if id == "human" || strings.HasPrefix(id, "ext:") {
			return SanitiseInline(id)
		}
		if len(id) > 8 {
			return "pane-" + id[:8]
		}
		return SanitiseInline(id)
	}
	rank := func(id string) int { // nodes in model order, outside blockers last
		if i, ok := order[id]; ok {
			return i
		}
		return len(order)
	}
	sortIDs := func(ids []string) {
		sort.SliceStable(ids, func(i, j int) bool {
			if rank(ids[i]) != rank(ids[j]) {
				return rank(ids[i]) < rank(ids[j])
			}
			return ids[i] < ids[j]
		})
	}
	for k := range waiters {
		sortIDs(waiters[k])
	}

	// waitersOf(ids) = distinct agents reachable through waiters, ids excluded.
	waitersOf := func(ids ...string) int {
		seen := map[string]bool{}
		for _, id := range ids {
			seen[id] = true
		}
		var stack []string
		for _, id := range ids {
			stack = append(stack, waiters[id]...)
		}
		n := 0
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[x] {
				continue
			}
			seen[x] = true
			n++
			stack = append(stack, waiters[x]...)
		}
		return n
	}
	waiting := func(id string) int { return waitersOf(id) }

	var build func(id, via string, onPath, drawn map[string]bool) FlowDepNode
	build = func(id, via string, onPath, drawn map[string]bool) FlowDepNode {
		d := FlowDepNode{ID: id, Label: label(id), Waiting: waiting(id)}
		if n := byID[id]; n != nil {
			d.Work, d.Activity, d.Health = n.Work, n.Activity, n.Health
		}
		for _, b := range blockedBy[id] {
			if b != via {
				d.Also = append(d.Also, label(b))
			}
		}
		if drawn[id] {
			d.Repeat = true
			return d
		}
		drawn[id] = true
		onPath[id] = true
		for _, w := range waiters[id] {
			if onPath[w] {
				continue // a cycle hanging off this root: listed under Cycles
			}
			d.Children = append(d.Children, build(w, id, onPath, drawn))
		}
		delete(onPath, id)
		return d
	}

	// Roots: every blocker that is not itself blocked.
	var roots []string
	for b := range waiters {
		if len(blockedBy[b]) == 0 {
			roots = append(roots, b)
		}
	}
	sortIDs(roots)
	for _, r := range roots {
		deps.Roots = append(deps.Roots, build(r, "", map[string]bool{}, map[string]bool{}))
	}
	sort.SliceStable(deps.Roots, func(i, j int) bool { return deps.Roots[i].Waiting > deps.Roots[j].Waiting })

	for _, ring := range flowDepCycles(blockedBy, sortIDs) {
		c := FlowDepCycle{Waiting: waitersOf(ring...)}
		for _, id := range ring {
			c.Members = append(c.Members, label(id))
		}
		deps.Cycles = append(deps.Cycles, c)
	}
	return deps
}

// flowDepCycles finds every ring of blocked_by edges, once each, starting from
// its first member in model order.
func flowDepCycles(blockedBy map[string][]string, sortIDs func([]string)) [][]string {
	ids := make([]string, 0, len(blockedBy))
	for id := range blockedBy {
		ids = append(ids, id)
	}
	sortIDs(ids)
	const (
		white = iota
		grey
		black
	)
	colour := map[string]int{}
	inCycle := map[string]bool{}
	var out [][]string
	var stack []string
	var visit func(n string)
	visit = func(n string) {
		colour[n] = grey
		stack = append(stack, n)
		for _, next := range blockedBy[n] {
			switch colour[next] {
			case white:
				visit(next)
			case grey:
				for i := len(stack) - 1; i >= 0; i-- {
					if stack[i] != next {
						continue
					}
					ring := stack[i:]
					dup := false
					for _, id := range ring {
						dup = dup || inCycle[id]
					}
					if !dup {
						for _, id := range ring {
							inCycle[id] = true
						}
						out = append(out, append([]string(nil), ring...))
					}
					break
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[n] = black
	}
	for _, id := range ids {
		if colour[id] == white {
			visit(id)
		}
	}
	return out
}

// RenderFlowDeps draws the dependency tree as plain lines (no styling), the
// same for the TUI's flow view and `rysh board tail --view flow`. Empty when
// nobody is blocked.
//
//	dependencies · 3 blocked · bottleneck: mgr-a (3 waiting)
//	  ▣ mgr-a {progress·working}  ← 3 waiting
//	    ├─ wk-a1 {blocked·idle}  ← 1 waiting
//	    │  └─ ceo {blocked·idle}
//	    └─ wk-a3 {blocked·idle}  · also blocked by human
//	  ⟳ DEADLOCK  a → b → a  · 1 waiting on it
func RenderFlowDeps(d FlowDeps) []string {
	if d.Blocked == 0 {
		return nil
	}
	head := "dependencies · " + itoa(d.Blocked) + " blocked"
	if b := d.Bottleneck(); b != nil {
		head += " · bottleneck: " + b.Label + " (" + itoa(b.Waiting) + " waiting)"
	}
	if len(d.Cycles) > 0 {
		head += " · " + itoa(len(d.Cycles)) + " deadlock"
		if len(d.Cycles) > 1 {
			head += "s"
		}
	}
	out := []string{head}
	var walk func(n FlowDepNode, prefix string, last bool)
	walk = func(n FlowDepNode, prefix string, last bool) {
		branch, next := "├─ ", "│  "
		if last {
			branch, next = "└─ ", "   "
		}
		out = append(out, prefix+branch+flowDepLine(n))
		if n.Repeat {
			return
		}
		for i, c := range n.Children {
			walk(c, prefix+next, i == len(n.Children)-1)
		}
	}
	for _, r := range d.Roots {
		out = append(out, "  ▣ "+flowDepLine(r))
		for i, c := range r.Children {
			walk(c, "    ", i == len(r.Children)-1)
		}
	}
	for _, c := range d.Cycles {
		line := "  ⟳ DEADLOCK  " + strings.Join(append(append([]string(nil), c.Members...), c.Members[0]), " → ")
		if c.Waiting > 0 {
			line += " · " + itoa(c.Waiting) + " waiting on it"
		}
		out = append(out, line)
	}
	return out
}

func flowDepLine(n FlowDepNode) string {
	s := n.Label
	var parts []string
	if n.Work != "" {
		parts = append(parts, n.Work)
	}
	switch {
	case n.Health != "" && n.Health != "ok":
		parts = append(parts, strings.ToUpper(n.Health))
	case n.Activity != "" && n.Activity != "unknown":
		parts = append(parts, n.Activity)
	}
	if len(parts) > 0 {
		s += " {" + strings.Join(parts, "·") + "}"
	}
	if n.Waiting > 0 {
		s += "  ← " + itoa(n.Waiting) + " waiting"
	}
	if len(n.Also) > 0 {
		s += "  · also blocked by " + strings.Join(n.Also, ", ")
	}
	if n.Repeat {
		s += "  (see above)"
	}
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }

// RenderFlowDepsFit draws the tree in at most maxRows lines. When the full
// tree does not fit it falls back to one line per root and per deadlock —
// each root with its direct waiters inline — because the roots ARE the
// answer to "where is the bottleneck", and a truncated tree hides the later
// roots and every deadlock (found on a live board: the human blocker and the
// deadlock were both below the cut). Only then is the list itself cut.
func RenderFlowDepsFit(d FlowDeps, maxRows int) []string {
	full := RenderFlowDeps(d)
	if len(full) <= maxRows || maxRows <= 0 {
		return full
	}
	out := []string{full[0]}
	for _, r := range d.Roots {
		line := "  ▣ " + flowDepLine(r)
		if len(r.Children) > 0 {
			names := make([]string, 0, len(r.Children))
			for _, c := range r.Children {
				names = append(names, c.Label) // direct waiters, repeats included
			}
			line += "  ⇐ " + strings.Join(names, ", ")
		}
		out = append(out, line)
	}
	for _, l := range full {
		if strings.HasPrefix(l, "  ⟳ ") {
			out = append(out, l)
		}
	}
	if len(out) > maxRows {
		more := len(out) - (maxRows - 1)
		out = append(out[:maxRows-1:maxRows-1], "  … "+itoa(more)+" more — see `rysh board tail --view flow`")
	}
	return out
}
