// SPDX-License-Identifier: Apache-2.0

package main

// The open-source export prunes commands_doc_test.go (docs/ is not exported), and
// family_cmd_test.go also reads the dispatch list, so the parser lives here.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// dispatchedCommands reads every top-level command name the binary answers to
// from main.go: the os.Args[1] == "x" checks in main() and the case labels of
// run()'s switch on args[0]. Read from the source, never listed here, so a new
// command cannot be added without this test seeing it.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	add := func(e ast.Expr) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if s, err := strconv.Unquote(v.Value); err == nil {
				names = append(names, s)
			}
		case *ast.Ident:
			if v.Name == "setupCommand" {
				names = append(names, setupCommand)
			}
		}
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		switch fn.Name.Name {
		case "main":
			ast.Inspect(fn, func(n ast.Node) bool {
				if b, ok := n.(*ast.BinaryExpr); ok && b.Op == token.EQL {
					if ix, ok := b.X.(*ast.IndexExpr); ok {
						if sel, ok := ix.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Args" {
							add(b.Y)
						}
					}
				}
				return true
			})
		case "run":
			for _, st := range fn.Body.List {
				sw, ok := st.(*ast.SwitchStmt)
				if !ok {
					continue
				}
				if ix, ok := sw.Tag.(*ast.IndexExpr); !ok || identName(ix.X) != "args" {
					continue
				}
				for _, c := range sw.Body.List {
					for _, e := range c.(*ast.CaseClause).List {
						add(e)
					}
				}
				break
			}
		}
	}
	return names
}

func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
