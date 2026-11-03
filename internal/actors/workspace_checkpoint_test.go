// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func checkpointRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
	} {
		gitRun(t, dir, args...)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runCP(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := runCheckpointCommand(context.Background(), &out, dir, "pane-1", args)
	return out.String(), err
}

func TestCheckpointCommand_SaveListRestoreUndo(t *testing.T) {
	dir := checkpointRepo(t)
	if out, err := runCP(t, dir, "save", "before", "edit"); err != nil || !strings.Contains(out, "#1") {
		t.Fatalf("save: %q %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCP(t, dir, "list")
	if err != nil || !strings.Contains(out, "before edit") {
		t.Fatalf("list: %q %v", out, err)
	}
	if out, err := runCP(t, dir, "diff"); err != nil || !strings.Contains(out, "a.txt") {
		t.Fatalf("diff: %q %v", out, err)
	}
	if out, err := runCP(t, dir, "undo"); err != nil || !strings.Contains(out, "restored") {
		t.Fatalf("undo: %q %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one\n" {
		t.Fatalf("a.txt = %q", b)
	}
	// The replaced state is restorable by number.
	if out, err := runCP(t, dir, "restore", "1"); err != nil || !strings.Contains(out, "restored") {
		t.Fatalf("restore 1: %q %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "two\n" {
		t.Fatalf("a.txt after restore = %q", b)
	}
}

func TestCheckpointCommand_Failures(t *testing.T) {
	dir := checkpointRepo(t)
	cases := []struct {
		dir  string
		args []string
		want string
	}{
		{t.TempDir(), []string{"list"}, "not inside a git work tree"},
		{dir, []string{"restore"}, "usage"},
		{dir, []string{"restore", "x"}, "usage"},
		{dir, []string{"restore", "3"}, "no such checkpoint"},
		{dir, []string{"undo"}, "no such checkpoint"},
		{dir, []string{"wibble"}, "usage"},
	}
	for _, c := range cases {
		out, err := runCP(t, c.dir, c.args...)
		if err == nil || !strings.Contains(out+err.Error(), c.want) {
			t.Errorf("%v: out=%q err=%v, want %q", c.args, out, err, c.want)
		}
	}
	if out, err := runCP(t, dir); err != nil || !strings.Contains(out, "no checkpoints") {
		t.Errorf("bare ##checkpoint lists: %q %v", out, err)
	}
}

func TestCheckpointCommand_IsInTheTable(t *testing.T) {
	spec, ok := ryshCommandIndex["checkpoint"]
	if !ok || !spec.statusAware {
		t.Fatal("##checkpoint must be a status-aware table entry")
	}
	if _, ok := ryshCommandIndex["undo"]; ok {
		t.Fatal("undo is a ##checkpoint subcommand and a CLI verb, not a ## word")
	}
}
