// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// fsWorkspace builds a workspace with a policy file and publishes its fs rules
// (design 068) for the duration of the test. doc == "" publishes no rules.
func fsWorkspace(t *testing.T, doc string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"README.md":      "hello world\n",
		"src/main.go":    "package main // hello\n",
		"docs/guide.md":  "hello guide\n",
		".env":           "TOKEN=hello\n",
		"secrets/db.txt": "hello secret\n",
	}
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ryshDir := filepath.Join(root, ".rysh")
	if err := os.MkdirAll(ryshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { policy.SetActiveFS(nil) })
	if doc == "" {
		policy.SetActiveFS(nil)
		return root
	}
	if err := os.WriteFile(filepath.Join(ryshDir, policy.FileName), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	p := policy.Load(ryshDir)
	if p.Err != nil {
		t.Fatal(p.Err)
	}
	policy.SetActiveFS(p.ResolveFS(root))
	return root
}

const fsTestPolicy = `
fs:
  read_only: ["**"]
  read_write: ["src/**"]
  deny: [".env", "secrets"]
`

func runFS(t *testing.T, tool ToolExecutor, params any) *ToolOutput {
	t.Helper()
	b, _ := json.Marshal(params)
	out, err := tool.Execute(context.Background(), b)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return out
}

func wantRefused(t *testing.T, out *ToolOutput, what string) {
	t.Helper()
	if out.ErrorKind != ErrKindPermissionDenied || !strings.Contains(out.Error, "policy: ") {
		t.Errorf("%s: want a policy refusal, got kind=%q err=%q content=%q", what, out.ErrorKind, out.Error, out.Content)
	}
}

func wantOK(t *testing.T, out *ToolOutput, what string) {
	t.Helper()
	if out.Error != "" {
		t.Errorf("%s: unexpected error %q (%s)", what, out.Error, out.ErrorKind)
	}
}

func TestFSPolicy_FileRead(t *testing.T) {
	root := fsWorkspace(t, fsTestPolicy)
	tool := NewFileReadTool(root)
	wantRefused(t, runFS(t, tool, map[string]any{"file_path": ".env"}), "read .env")
	wantRefused(t, runFS(t, tool, map[string]any{"file_path": "secrets/db.txt"}), "read secrets/")
	wantRefused(t, runFS(t, tool, map[string]any{"file_path": "src/../.env"}), "read src/../.env")
	wantOK(t, runFS(t, tool, map[string]any{"file_path": "docs/guide.md"}), "read a read_only file")
}

func TestFSPolicy_FileWrite(t *testing.T) {
	root := fsWorkspace(t, fsTestPolicy)
	tool := NewFileWriteTool(root)
	wantRefused(t, runFS(t, tool, map[string]any{"file_path": "docs/new.md", "content": "x"}), "write into read_only")
	wantRefused(t, runFS(t, tool, map[string]any{"file_path": filepath.Join(root, "secrets/new"), "content": "x"}), "write into deny")
	if _, err := os.Stat(filepath.Join(root, "docs/new.md")); !os.IsNotExist(err) {
		t.Fatal("a refused write must not create the file")
	}
	wantOK(t, runFS(t, tool, map[string]any{"file_path": "src/new.go", "content": "package main\n"}), "write in the read_write carve-out")
}

func TestFSPolicy_Edit(t *testing.T) {
	root := fsWorkspace(t, fsTestPolicy)
	tool := NewEditTool(root)
	wantRefused(t, runFS(t, tool, map[string]any{"file_path": "README.md", "old_string": "hello", "new_string": "bye"}), "edit read_only")
	wantRefused(t, runFS(t, tool, map[string]any{"file_path": ".env", "old_string": "hello", "new_string": "bye"}), "edit deny")
	if b, _ := os.ReadFile(filepath.Join(root, "README.md")); string(b) != "hello world\n" {
		t.Fatal("a refused edit must not change the file")
	}
	wantOK(t, runFS(t, tool, map[string]any{"file_path": "src/main.go", "old_string": "hello", "new_string": "bye"}), "edit in carve-out")
}

func TestFSPolicy_SymlinkIntoDenied(t *testing.T) {
	root := fsWorkspace(t, fsTestPolicy)
	if err := os.Symlink(filepath.Join(root, "secrets"), filepath.Join(root, "src/link")); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, runFS(t, NewFileReadTool(root), map[string]any{"file_path": "src/link/db.txt"}), "read through a symlink")
	wantRefused(t, runFS(t, NewFileWriteTool(root), map[string]any{"file_path": "src/link/new", "content": "x"}), "write through a symlink")
}

func TestFSPolicy_GlobAndGrepHideDenied(t *testing.T) {
	root := fsWorkspace(t, fsTestPolicy)
	g := runFS(t, NewGlobTool(root), map[string]any{"pattern": "**/*"})
	wantOK(t, g, "glob")
	if strings.Contains(g.Content, "secrets") || strings.Contains(g.Content, "db.txt") {
		t.Errorf("glob must hide denied paths:\n%s", g.Content)
	}
	if !strings.Contains(g.Content, "guide.md") {
		t.Errorf("glob must still list allowed files:\n%s", g.Content)
	}
	g2 := runFS(t, NewGlobTool(root), map[string]any{"pattern": "*", "path": "secrets"})
	wantRefused(t, g2, "glob rooted in a denied dir")

	// .env is hidden by grep's own dot-dir rule only for dirs; check files.
	r := runFS(t, NewGrepTool(root), map[string]any{"pattern": "hello"})
	wantOK(t, r, "grep")
	if strings.Contains(r.Content, "secrets") || strings.Contains(r.Content, ".env") {
		t.Errorf("grep must hide denied files:\n%s", r.Content)
	}
	if !strings.Contains(r.Content, "guide.md") {
		t.Errorf("grep must still search allowed files:\n%s", r.Content)
	}
	wantRefused(t, runFS(t, NewGrepTool(root), map[string]any{"pattern": "hello", "path": ".env"}), "grep a denied file")
}

// Rules only restrict: with no fs rules every tool behaves as before.
func TestFSPolicy_NoRulesIsToday(t *testing.T) {
	root := fsWorkspace(t, "")
	wantOK(t, runFS(t, NewFileReadTool(root), map[string]any{"file_path": ".env"}), "read .env")
	wantOK(t, runFS(t, NewFileWriteTool(root), map[string]any{"file_path": "docs/new.md", "content": "x"}), "write docs")
	wantOK(t, runFS(t, NewEditTool(root), map[string]any{"file_path": "secrets/db.txt", "old_string": "hello", "new_string": "bye"}), "edit secrets")
	g := runFS(t, NewGlobTool(root), map[string]any{"pattern": "**/*"})
	if !strings.Contains(g.Content, "db.txt") {
		t.Errorf("glob without rules must list everything:\n%s", g.Content)
	}
	r := runFS(t, NewGrepTool(root), map[string]any{"pattern": "bye|hello"})
	if !strings.Contains(r.Content, "secrets") || !strings.Contains(r.Content, ".env") {
		t.Errorf("grep without rules must search everything:\n%s", r.Content)
	}
}

func TestFSPolicy_Bash(t *testing.T) {
	root := fsWorkspace(t, fsTestPolicy)
	bash := NewBashTool(root, 10*time.Second, nil)
	wantRefused(t, runFS(t, bash, map[string]any{"command": "cat .env"}), "bash cat .env")
	wantRefused(t, runFS(t, bash, map[string]any{"command": "echo x > docs/new.md"}), "bash redirect into read_only")
	if _, err := os.Stat(filepath.Join(root, "docs/new.md")); !os.IsNotExist(err) {
		t.Fatal("a refused bash command must not run")
	}
	wantRefused(t, runFS(t, bash, map[string]any{"command": "ls", "work_dir": filepath.Join(root, "secrets")}), "bash with cwd in a denied dir")
	wantOK(t, runFS(t, bash, map[string]any{"command": "echo x > src/new.go"}), "bash write in carve-out")

	bg := NewBashBackgroundTool(root, NewBackgroundSessionManager())
	wantRefused(t, runFS(t, bg, map[string]any{"command": "tail -f secrets/db.txt"}), "bash_background on a denied path")
}

func TestFSPolicy_BashNoRulesIsToday(t *testing.T) {
	root := fsWorkspace(t, "")
	bash := NewBashTool(root, 10*time.Second, nil)
	out := runFS(t, bash, map[string]any{"command": "cat .env && echo x > docs/new.md"})
	wantOK(t, out, "bash without rules")
	if !strings.Contains(out.Content, "TOKEN=hello") {
		t.Errorf("bash without rules must run unchanged: %q", out.Content)
	}
}
