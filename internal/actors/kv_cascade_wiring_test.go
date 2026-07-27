package actors

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Guard for the KV cascade wiring.
//
// Every level of the cascade falls back to the old direct (racy) read when the
// child does not answer. That is deliberate — losing state is worse than a
// racy write — but it also means a BROKEN cascade is invisible: delete a case
// arm and persistence keeps working, silently back on the racy path, with no
// test failure and no log line.
//
// These checks make that regression loud. They are structural rather than
// behavioural because spawning the real actors requires a live NATS bridge
// (PaneGroupActor.Started dereferences g.pub/g.nc), which is far more machinery
// than a one-line case arm warrants.
func TestKVCascadeArmsAreWired(t *testing.T) {
	cases := []struct {
		file    string
		reqType string
		who     string
	}{
		{"pane_group.go", "paneGroupKVRequest", "PaneGroupActor"},
		{"lane.go", "laneKVRequest", "LaneActor"},
		{"tab.go", "tabKVRequest", "TabActor"},
	}

	for _, c := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, c.file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", c.file, err)
		}

		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range cc.List {
				star, ok := expr.(*ast.StarExpr)
				if !ok {
					continue
				}
				if id, ok := star.X.(*ast.Ident); ok && id.Name == c.reqType {
					found = true
				}
			}
			return true
		})

		if !found {
			t.Errorf("%s (%s) no longer handles *%s — the KV cascade would silently "+
				"fall back to the racy direct read on every persist",
				c.who, c.file, c.reqType)
		}
	}
}

// The workspace entry point must serialise tabs through tabKVFor (which runs
// the cascade), not by calling ToKV() on the tab actor directly.
func TestPersistUsesCascadeEntryPoint(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "workspace_snapshot.go", nil, 0)
	if err != nil {
		t.Fatalf("parse workspace_snapshot.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if ok && fd.Name.Name == "persistToKVNow" {
			fn = fd
		}
		return true
	})
	if fn == nil {
		t.Fatal("persistToKVNow not found — update this guard to match the new structure")
	}

	usesCascade := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "tabKVFor":
			usesCascade = true
		case "ToKV":
			t.Errorf("%s: persistToKVNow calls ToKV() directly — that is the "+
				"cross-actor read the cascade exists to remove; go through tabKVFor",
				fset.Position(call.Pos()))
		}
		return true
	})

	if !usesCascade {
		t.Error("persistToKVNow no longer serialises tabs via tabKVFor")
	}
}
