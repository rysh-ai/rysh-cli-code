// SPDX-License-Identifier: Apache-2.0

package channels

// security-lead (2026-11-01): NewEmailAdapterWithDialerForTest is a code-only
// seam for tests (plaintext IMAP to a loopback fake). This test fails if any
// non-test Go file in the module uses it, so no production path can reach a
// plaintext IMAP dial.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmailDialerSeamIsTestOnly(t *testing.T) {
	const seam = "NewEmailAdapterWithDialerForTest"
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	var files int
	var callers []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata", "out":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil // not this test's business
		}
		// Any use counts, not only a call (a function value passed on is a
		// path too); only the declaration's own name is allowed.
		decl := map[*ast.Ident]bool{}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == seam {
				decl[fd.Name] = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == seam && !decl[id] {
				callers = append(callers, path)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Guard against a vacuous pass: the walk must have seen the module,
	// including the file that defines the seam.
	if files < 100 {
		t.Fatalf("walked only %d non-test Go files under %s; the module was not scanned", files, root)
	}
	if len(callers) != 0 {
		t.Fatalf("%s is test-only, but non-test files use it: %v", seam, callers)
	}
}
