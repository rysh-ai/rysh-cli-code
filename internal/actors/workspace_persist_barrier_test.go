// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// Guard for the persist-ordering invariant.
//
// forwardToActiveTab() is a fire-and-forget NATS Send: the tab actor applies the
// mutation later, on its own goroutine. persistToKV() serialises tab state via a
// DIRECT cross-actor ToKV() read, so calling it straight after a forward can
// persist the PRE-mutation state and then clear the dirty flag — silently
// reverting the change on a crash. (That was the layout-resize drift bug.)
//
// Two things make an immediate persist safe:
//
//   - syncActivePane() in between. It issues a synchronous Request to
//     rysh.tab.{id}.inbox — the SAME subject forwardToActiveTab() published to.
//     NATS preserves per-subject order from one connection and the tab has a
//     single subscription feeding a FIFO mailbox, so the reply cannot come back
//     until the forwarded message has been processed. It is a real barrier.
//
//   - otherwise, persistToKVDeferred(), which skips the leading-edge write and
//     lets the trailing flush persist the applied state.
//
// This test fails if a handler forwards then persists immediately with no
// barrier — including if someone "tidies away" a syncActivePane() call that is
// load-bearing for persistence correctness rather than just for focus tracking.
func TestAsyncForwardMustBarrierOrDeferPersist(t *testing.T) {
	const (
		forward = "forwardToActiveTab"
		barrier = "syncActivePane"
		persist = "persistToKV"
		defer_  = "persistToKVDeferred"
	)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "workspace.go", nil, 0)
	if err != nil {
		t.Fatalf("parse workspace.go: %v", err)
	}

	type call struct {
		pos  token.Pos
		name string
	}

	// Collect w.<method>() calls inside a node, in source order.
	calls := func(n ast.Node) []call {
		var out []call
		ast.Inspect(n, func(x ast.Node) bool {
			ce, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := ce.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "w" {
				return true
			}
			switch sel.Sel.Name {
			case forward, barrier, persist, defer_:
				out = append(out, call{ce.Pos(), sel.Sel.Name})
			}
			return true
		})
		sort.Slice(out, func(i, j int) bool { return out[i].pos < out[j].pos })
		return out
	}

	checked := 0
	ast.Inspect(f, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		seq := calls(cc)

		// Walk the sequence; after each forward, the next persist-ish call must
		// either be preceded by a barrier or be the deferred variant.
		sawForward := false
		sawBarrier := false
		for _, c := range seq {
			switch c.name {
			case forward:
				sawForward, sawBarrier = true, false
			case barrier:
				sawBarrier = true
			case defer_:
				sawForward, sawBarrier = false, false // always safe
			case persist:
				if sawForward && !sawBarrier {
					t.Errorf("%s: forwardToActiveTab() then persistToKV() with no "+
						"syncActivePane() barrier — this persists PRE-mutation state and "+
						"clears the dirty flag. Use persistToKVDeferred() (or add a barrier).",
						fset.Position(c.pos))
				}
				sawForward, sawBarrier = false, false
			}
			if c.name == forward {
				checked++
			}
		}
		return true
	})

	// Sanity: the analysis must actually be looking at something. If the file is
	// refactored so these calls move out of case clauses, this test would
	// silently pass while checking nothing.
	if checked == 0 {
		t.Fatal("found no forwardToActiveTab() calls in case clauses — the guard is " +
			"no longer analysing anything; update it to match the new structure")
	}
	t.Logf("checked %d forwardToActiveTab call sites", checked)
}
