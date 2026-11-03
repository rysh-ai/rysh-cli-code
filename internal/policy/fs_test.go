// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFSPolicy(t *testing.T, doc string) *Policy {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(dir)
}

// realTemp returns a temp dir with symlinks resolved (macOS /var → /private/var),
// so expected paths compare equal.
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFS_LoadValid(t *testing.T) {
	p := loadFSPolicy(t, `
fs:
  read_only: ["**"]
  read_write: ["src/**", out]
  deny: [".env", "$HOME/.ssh"]
`)
	if p.Err != nil || !p.Loaded {
		t.Fatalf("load: Err=%v Loaded=%v", p.Err, p.Loaded)
	}
	if len(p.FS.ReadOnly) != 1 || len(p.FS.ReadWrite) != 2 || len(p.FS.Deny) != 2 {
		t.Fatalf("fs = %+v", p.FS)
	}
}

// A policy with no fs: section has no fs rules at all: today's behaviour.
func TestFS_MissingSectionIsToday(t *testing.T) {
	p := loadFSPolicy(t, "bash:\n  deny: [rm]\n")
	if p.Err != nil {
		t.Fatal(p.Err)
	}
	if r := p.ResolveFS(realTemp(t)); r != nil {
		t.Fatalf("no fs section must resolve to nil rules, got %+v", r)
	}
	if r := Empty().ResolveFS(realTemp(t)); r != nil {
		t.Fatal("empty policy must resolve to nil rules")
	}
	var nilRules *FSRules
	if err := nilRules.Check("/etc/passwd", FSWrite); err != nil {
		t.Fatalf("nil rules must allow everything: %v", err)
	}
}

func TestFS_LoaderFailsClosed(t *testing.T) {
	bad := map[string]string{
		"unknown key":   "fs:\n  read_olny: [x]\n",
		"empty glob":    "fs:\n  deny: [\"\"]\n",
		"dotdot":        "fs:\n  deny: [\"a/../b\"]\n",
		"bracket":       "fs:\n  deny: [\"a[0-9]\"]\n",
		"brace":         "fs:\n  deny: [\"*.{go,md}\"]\n",
		"tilde user":    "fs:\n  deny: [\"~root/x\"]\n",
		"backslash":     "fs:\n  deny: [\"a\\\\b\"]\n",
		"not a list":    "fs:\n  deny: x\n",
		"not a mapping": "fs: [x]\n",
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			p := loadFSPolicy(t, doc+"bash:\n  allow: [ls]\n")
			if p.Err == nil {
				t.Fatalf("expected fail-closed error for %q", doc)
			}
			if len(p.BashAllow) != 0 || len(p.FS.Deny) != 0 {
				t.Fatalf("broken policy must apply no rules: %+v", p)
			}
		})
	}
}

func TestFS_Verdicts(t *testing.T) {
	root := realTemp(t)
	p := loadFSPolicy(t, `
fs:
  read_only: ["**", "src/gen"]
  read_write: ["src/**", out]
  deny: [".env", "secrets", "src/**/*.key"]
`)
	if p.Err != nil {
		t.Fatal(p.Err)
	}
	r := p.ResolveFS(root)
	j := func(s string) string { return filepath.Join(root, s) }
	cases := []struct {
		path   string
		access FSAccess
		ok     bool
		rule   string
	}{
		{j("README.md"), FSRead, true, ""},
		{j("README.md"), FSWrite, false, "fs.read_only[0]"},
		{j("src/main.go"), FSWrite, true, ""},
		{j("src"), FSWrite, true, ""},
		{j("src/gen/x.go"), FSWrite, false, "fs.read_only[1]"}, // more literal ro beats rw
		{j("src/gen/x.go"), FSRead, true, ""},
		{j("out/a/b.txt"), FSWrite, true, ""}, // a directory rule covers what is beneath it
		{j(".env"), FSRead, false, "fs.deny[0]"},
		{j("secrets/db/pw"), FSRead, false, "fs.deny[1]"},
		{j("src/a/b/c.key"), FSWrite, false, "fs.deny[2]"}, // deny beats the rw carve-out
		{"/elsewhere/file", FSWrite, true, ""},             // no rule: today's behaviour
		{j("src/../.env"), FSRead, false, "fs.deny[0]"},
	}
	for _, c := range cases {
		err := r.Check(c.path, c.access)
		if c.ok && err != nil {
			t.Errorf("%s %v: unexpected refusal %v", c.path, c.access, err)
		}
		if !c.ok {
			var d *FSDeniedError
			if !errors.As(err, &d) {
				t.Errorf("%s %v: expected FSDeniedError, got %v", c.path, c.access, err)
				continue
			}
			if d.Rule != c.rule {
				t.Errorf("%s %v: rule %q, want %q", c.path, c.access, d.Rule, c.rule)
			}
			if !strings.HasPrefix(err.Error(), "policy: ") {
				t.Errorf("message must say policy: %q", err.Error())
			}
		}
	}
}

func TestFS_TieGoesToReadOnly(t *testing.T) {
	root := realTemp(t)
	p := loadFSPolicy(t, "fs:\n  read_only: [docs]\n  read_write: [docs]\n")
	if err := p.ResolveFS(root).Check(filepath.Join(root, "docs/a"), FSWrite); err == nil {
		t.Fatal("a tie between read_only and read_write must go to read_only")
	}
}

func TestFS_HomeExpansion(t *testing.T) {
	home := realTemp(t)
	t.Setenv("HOME", home)
	p := loadFSPolicy(t, "fs:\n  deny: [\"$HOME/.ssh\", \"~/.aws\"]\n")
	r := p.ResolveFS(realTemp(t))
	for _, s := range []string{".ssh/id_ed25519", ".aws/credentials"} {
		if err := r.Check(filepath.Join(home, s), FSRead); err == nil {
			t.Errorf("%s should be denied", s)
		}
	}
	if err := r.Check(filepath.Join(home, "notes.txt"), FSRead); err != nil {
		t.Errorf("unrelated home file refused: %v", err)
	}
}

func TestFS_SymlinksResolved(t *testing.T) {
	root := realTemp(t)
	if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secrets/pw"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Open name → denied target.
	if err := os.Symlink(filepath.Join(root, "secrets"), filepath.Join(root, "innocent")); err != nil {
		t.Fatal(err)
	}
	// Read-only name → open target: writing through it must still be refused.
	outside := realTemp(t)
	if err := os.Symlink(outside, filepath.Join(root, "docs/escape")); err != nil {
		t.Fatal(err)
	}
	p := loadFSPolicy(t, "fs:\n  read_only: [docs]\n  deny: [secrets]\n")
	r := p.ResolveFS(root)
	if err := r.Check(filepath.Join(root, "innocent/pw"), FSRead); err == nil {
		t.Error("symlink into a denied dir must be refused")
	}
	if err := r.Check(filepath.Join(root, "innocent/new-file"), FSWrite); err == nil {
		t.Error("a not-yet-existing file under a symlinked denied dir must be refused")
	}
	if err := r.Check(filepath.Join(root, "docs/escape/x"), FSWrite); err == nil {
		t.Error("writing through a symlink inside a read_only dir must be refused")
	}
	if err := r.Check(filepath.Join(outside, "x"), FSWrite); err != nil {
		t.Errorf("the target dir itself is not covered by any rule: %v", err)
	}
}

// The workspace root reached through a symlink still matches root-relative rules.
func TestFS_RootViaSymlink(t *testing.T) {
	real := realTemp(t)
	link := filepath.Join(realTemp(t), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	p := loadFSPolicy(t, "fs:\n  deny: [.env]\n")
	r := p.ResolveFS(link)
	for _, path := range []string{filepath.Join(link, ".env"), filepath.Join(real, ".env")} {
		if err := r.Check(path, FSRead); err == nil {
			t.Errorf("%s should be denied", path)
		}
	}
}

func TestFS_OrgMergeStrictest(t *testing.T) {
	root := realTemp(t)
	org := loadFSPolicy(t, "fs:\n  read_only: [infra]\n  deny: [.env]\n")
	proj := loadFSPolicy(t, "fs:\n  read_write: [infra, \"infra/**\"]\n  deny: [secrets]\n")
	m := Merge(org, proj)
	if m.Err != nil {
		t.Fatal(m.Err)
	}
	r := m.ResolveFS(root)
	if err := r.Check(filepath.Join(root, "infra/main.tf"), FSWrite); err == nil {
		t.Error("a project read_write must not carve out of an org read_only")
	} else if d := (*FSDeniedError)(nil); errors.As(err, &d) && d.Rule != "org.fs.read_only[0]" {
		t.Errorf("rule = %q, want org.fs.read_only[0]", d.Rule)
	}
	for _, s := range []string{".env", "secrets/x"} {
		if err := r.Check(filepath.Join(root, s), FSRead); err == nil {
			t.Errorf("%s: both files' denies apply", s)
		}
	}
	// Org-only fs, no project fs: org rules stand alone.
	m2 := Merge(org, loadFSPolicy(t, "bash:\n  allow: [ls]\n"))
	if err := m2.ResolveFS(root).Check(filepath.Join(root, ".env"), FSRead); err == nil {
		t.Error("org deny must apply with a project file that has no fs section")
	}
}

func TestFS_Mounts(t *testing.T) {
	root := realTemp(t)
	home := realTemp(t)
	t.Setenv("HOME", home)
	p := loadFSPolicy(t, "fs:\n  read_only: [docs]\n  read_write: [src]\n  deny: [\"$HOME/.ssh\"]\n")
	m := p.ResolveFS(root).Mounts()
	if len(m.ReadOnly) != 1 || m.ReadOnly[0] != filepath.Join(root, "docs") {
		t.Errorf("read_only mounts = %v", m.ReadOnly)
	}
	if len(m.ReadWrite) != 1 || m.ReadWrite[0] != filepath.Join(root, "src") {
		t.Errorf("read_write mounts = %v", m.ReadWrite)
	}
	if len(m.Deny) != 1 || m.Deny[0] != filepath.Join(home, ".ssh") {
		t.Errorf("deny mounts = %v", m.Deny)
	}
	var nilRules *FSRules
	if m := nilRules.Mounts(); len(m.ReadOnly)+len(m.ReadWrite)+len(m.Deny) != 0 {
		t.Error("nil rules have no mounts")
	}
}

func TestFS_ActiveGlobal(t *testing.T) {
	root := realTemp(t)
	t.Cleanup(func() { SetActiveFS(nil) })
	SetActiveFS(nil)
	if err := CheckFS(filepath.Join(root, ".env"), FSRead); err != nil {
		t.Fatalf("no active rules must allow: %v", err)
	}
	SetActiveFS(loadFSPolicy(t, "fs:\n  deny: [.env]\n").ResolveFS(root))
	if err := CheckFS(filepath.Join(root, ".env"), FSRead); err == nil {
		t.Fatal("active deny must refuse")
	}
	if ActiveFS() == nil {
		t.Fatal("ActiveFS must return the published rules")
	}
}

func TestFS_SummaryShowsRules(t *testing.T) {
	p := loadFSPolicy(t, "fs:\n  read_only: [\"**\"]\n  read_write: [src]\n  deny: [.env]\n")
	s := p.Summary()
	for _, want := range []string{"fs read-only:", "fs read-write:", "fs deny:", ".env", "src"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
}
