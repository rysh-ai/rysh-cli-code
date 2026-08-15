// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBuildEnvBlock_EmptyTemplate returns "" so an empty/missing env-block
// file effectively disables the feature.
func TestBuildEnvBlock_EmptyTemplate(t *testing.T) {
	if got := buildEnvBlock("", "/tmp"); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
	if got := buildEnvBlock("   \n  ", "/tmp"); got != "" {
		t.Errorf("blank template should also yield empty, got %q", got)
	}
}

// TestBuildEnvBlock_Substitution verifies the simple {{var}} substitutions
// without relying on git being on PATH (we expect "<no git>" in a temp dir).
func TestBuildEnvBlock_Substitution(t *testing.T) {
	dir := t.TempDir()
	// Mark this directory as a Go module so project_type detection has
	// something to find.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("seed go.mod: %v", err)
	}
	template := "cwd={{cwd}}\nos={{os}}\narch={{arch}}\ndate={{date}}\nbranch={{git_branch}}{{git_dirty}}\ntype={{project_type}}\ntree={{tree}}\n"
	out := buildEnvBlock(template, dir)
	if !strings.Contains(out, "cwd="+dir) {
		t.Errorf("missing cwd substitution: %q", out)
	}
	if !strings.Contains(out, "os="+runtime.GOOS) {
		t.Errorf("missing os substitution: %q", out)
	}
	if !strings.Contains(out, "arch="+runtime.GOARCH) {
		t.Errorf("missing arch substitution: %q", out)
	}
	if !strings.Contains(out, "type=Go module") {
		t.Errorf("expected 'Go module' project_type, got: %q", out)
	}
	// Tree should include go.mod entry.
	if !strings.Contains(out, "go.mod") {
		t.Errorf("expected go.mod in tree, got: %q", out)
	}
}

// TestDetectProjectType covers the marker precedence: go.mod > package.json > etc.
func TestDetectProjectType(t *testing.T) {
	dir := t.TempDir()
	if got := detectProjectType(dir); got != "unrecognised" {
		t.Errorf("empty dir = %q, want 'unrecognised'", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectProjectType(dir); got != "Node.js / npm" {
		t.Errorf("with package.json = %q", got)
	}
	// go.mod outranks package.json by order in the table.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectProjectType(dir); got != "Go module" {
		t.Errorf("with go.mod + package.json = %q", got)
	}
}

// TestShallowTree covers the basic listing behaviour and the "skip hidden"
// rule (".git" is skipped; ".github" survives as a special case).
func TestShallowTree(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "internal", "pkg"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644))

	tree := shallowTree(dir, 2, 50)
	if !strings.Contains(tree, "internal/") {
		t.Errorf("missing internal/, got:\n%s", tree)
	}
	if !strings.Contains(tree, "main.go") {
		t.Errorf("missing main.go, got:\n%s", tree)
	}
	if !strings.Contains(tree, ".github/") {
		t.Errorf("missing .github/, got:\n%s", tree)
	}
	if strings.Contains(tree, ".git/") {
		t.Errorf("should skip .git/, got:\n%s", tree)
	}
	if strings.Contains(tree, "node_modules") {
		t.Errorf("should skip node_modules/, got:\n%s", tree)
	}
}
