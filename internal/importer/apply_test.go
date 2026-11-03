// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// snapshot reads every regular file under root (relative path -> bytes).
func snapshot(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			out[rel] = data
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func keys(m map[string][]byte) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sameTree(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !bytes.Equal(v, w) {
			return false
		}
	}
	return true
}

var t0 = time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)

func TestApplyThenUndoRestoresExactly(t *testing.T) {
	root := t.TempDir()
	rysh := filepath.Join(root, ".rysh")
	if err := os.MkdirAll(filepath.Join(rysh, "imports"), 0o755); err != nil {
		t.Fatal(err)
	}
	// One file the import will overwrite, one it leaves alone.
	existing := filepath.Join(rysh, "imports", "tmux-api.rysh")
	if err := os.WriteFile(existing, []byte("old script\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rysh, "policy.yaml"), []byte("bash:\n  deny: [rm]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)

	changes := []Change{
		{Path: ".rysh/imports/tmux-api.rysh", Data: []byte("new script\n")},
		{Path: ".rysh/imports/tmux-notes.rysh", Data: []byte("created\n")},
	}
	m, err := Apply(root, "tmux", changes, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 {
		t.Fatalf("manifest files = %+v", m.Files)
	}
	if got, _ := os.ReadFile(existing); string(got) != "new script\n" {
		t.Fatalf("apply did not write: %q", got)
	}
	if _, err := os.Stat(filepath.Join(rysh, "imports", "tmux-notes.rysh")); err != nil {
		t.Fatalf("apply did not create the new file: %v", err)
	}

	um, err := Undo(root)
	if err != nil {
		t.Fatal(err)
	}
	if um.Source != "tmux" {
		t.Errorf("undo reports source %q", um.Source)
	}
	after := snapshot(t, root)
	// The backup folder stays as a record; everything else is byte-identical.
	for k := range after {
		if strings.HasPrefix(k, filepath.Join(".rysh", BackupDirName)) {
			delete(after, k)
		}
	}
	if !sameTree(before, after) {
		t.Errorf("undo did not restore the tree.\n before %q\n after  %q", keys(before), keys(after))
	}
	if _, err := Undo(root); err == nil {
		t.Error("a second undo with nothing left to undo must be an error")
	}
}

func TestUndoRefusesWhenFileChangedSinceImport(t *testing.T) {
	root := t.TempDir()
	if _, err := Apply(root, "zellij", []Change{{Path: ".rysh/imports/zellij-web.rysh", Data: []byte("v1\n")}}, t0); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, ".rysh", "imports", "zellij-web.rysh")
	if err := os.WriteFile(p, []byte("edited by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Undo(root); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("undo over a hand edit must refuse, got %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "edited by hand\n" {
		t.Error("a refused undo must not touch the file")
	}
	if _, err := UndoForce(root); err != nil {
		t.Fatalf("forced undo: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("forced undo must remove the file the import created")
	}
}

func TestUndoTakesTheLastImportOnly(t *testing.T) {
	root := t.TempDir()
	if _, err := Apply(root, "tmux", []Change{{Path: ".rysh/imports/a.rysh", Data: []byte("a\n")}}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(root, "zellij", []Change{{Path: ".rysh/imports/b.rysh", Data: []byte("b\n")}}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	m, err := Undo(root)
	if err != nil || m.Source != "zellij" {
		t.Fatalf("first undo = %+v, %v; want the zellij import", m, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".rysh/imports/a.rysh")); err != nil {
		t.Error("undoing the last import must leave the earlier one in place")
	}
	if m, err := Undo(root); err != nil || m.Source != "tmux" {
		t.Fatalf("second undo = %+v, %v; want the tmux import", m, err)
	}
}

func TestApplyRefusesPathsOutsideRysh(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{"../escape", "/etc/passwd", ".rysh/../x", "notrysh/file"} {
		if _, err := Apply(root, "tmux", []Change{{Path: bad, Data: []byte("x")}}, t0); err == nil {
			t.Errorf("Apply(%q) = nil error; only paths under .rysh/ may be written", bad)
		}
	}
	if s := snapshot(t, root); len(s) != 0 {
		t.Errorf("a refused apply wrote files: %q", keys(s))
	}
}

func TestApplyNothingIsAnError(t *testing.T) {
	if _, err := Apply(t.TempDir(), "tmux", nil, t0); err == nil {
		t.Error("an apply with no changes must say so, not write an empty backup")
	}
}

func TestBackupDirIsGitIgnored(t *testing.T) {
	root := t.TempDir()
	if _, err := Apply(root, "tmux", []Change{{Path: ".rysh/imports/a.rysh", Data: []byte("a\n")}}, t0); err != nil {
		t.Fatal(err)
	}
	gi, err := os.ReadFile(filepath.Join(root, ".rysh", BackupDirName, ".gitignore"))
	if err != nil || strings.TrimSpace(string(gi)) != "*" {
		t.Errorf("backup dir .gitignore = %q, %v; want \"*\" so backups never reach git", gi, err)
	}
}

// Preview must be read-only: building a plan and its change list for a
// project leaves the project's files untouched.
func TestPreviewWritesNothing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	p, err := ParseTmuxLive(string(readFixture(t, "tmux-list-panes.txt")), fixtureHome, "")
	if err != nil {
		t.Fatal(err)
	}
	p.Context = ContextFiles(root)
	_ = p.Text()
	if _, err := p.JSON(); err != nil {
		t.Fatal(err)
	}
	if ch := p.Changes(root); len(ch) == 0 {
		t.Fatal("a layout plan must produce a script change")
	}
	if after := snapshot(t, root); !sameTree(before, after) {
		t.Errorf("preview wrote files: %q", keys(after))
	}
}

func TestChangesNameTheScriptBySourceAndName(t *testing.T) {
	p := &Plan{Source: "tmux", Name: "My Project!", Tabs: []Tab{{Name: "a", Lanes: []Lane{{Panes: []Pane{{}}}}}}}
	ch := p.Changes(t.TempDir())
	if len(ch) != 1 || ch[0].Path != ".rysh/imports/tmux-my-project.rysh" {
		t.Fatalf("changes = %+v", ch)
	}
}
