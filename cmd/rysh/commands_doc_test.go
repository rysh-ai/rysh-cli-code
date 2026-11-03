// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
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

// Every command the binary dispatches has its line in docs/commands.md.
func TestCommandsDocListsEveryCommand(t *testing.T) {
	doc, err := os.ReadFile("../../docs/commands.md")
	if err != nil {
		t.Fatal(err)
	}
	names := dispatchedCommands(t)
	// Non-empty first: a parser that finds nothing would pass vacuously.
	if len(names) < 40 {
		t.Fatalf("read only %d commands from main.go: %v", len(names), names)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "-") {
			continue // flag aliases of version/help
		}
		// `rysh NAME …`, or NAME inside a `rysh a\|NAME\|b …` alternation (the
		// table escapes its pipes).
		const alt = `\\\|`
		line := regexp.MustCompile("`rysh ([a-z-]+" + alt + ")*" + regexp.QuoteMeta(n) + "([` ]|" + alt + ")")
		if !line.Match(doc) {
			t.Errorf("docs/commands.md has no line for `rysh %s`", n)
		}
	}
}
