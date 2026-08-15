// SPDX-License-Identifier: Apache-2.0

package browserinstance

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureProfileCreatesDirAndRegistry(t *testing.T) {
	work := t.TempDir()

	dir, err := EnsureProfile(work, "work")
	if err != nil {
		t.Fatalf("EnsureProfile: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("profile dir not created: %v", err)
	}
	want := filepath.Join(RootDir(work), "work")
	if absWant, _ := filepath.Abs(want); dir != absWant {
		t.Fatalf("profile dir = %q, want %q", dir, absWant)
	}

	regs, err := LoadRegistry(work)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if len(regs) != 1 || regs[0].Name != "work" {
		t.Fatalf("registry = %+v, want one entry named 'work'", regs)
	}
	if regs[0].CreatedMs == 0 {
		t.Fatalf("CreatedMs not set")
	}
}

func TestEnsureProfileIdempotent(t *testing.T) {
	work := t.TempDir()
	if _, err := EnsureProfile(work, "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureProfile(work, "work"); err != nil {
		t.Fatal(err)
	}
	regs, _ := LoadRegistry(work)
	if len(regs) != 1 {
		t.Fatalf("registry has %d entries, want 1 (idempotent)", len(regs))
	}
}

func TestSanitizeProfile(t *testing.T) {
	cases := map[string]string{
		"work":             "work",
		"  spaced  ":       "spaced",
		"../escape":        "escape",
		"a/b/c":            "c",
		"..":               DefaultProfile,
		"":                 DefaultProfile,
		".":                DefaultProfile,
		"../../etc/passwd": "passwd",
	}
	for in, want := range cases {
		if got := SanitizeProfile(in); got != want {
			t.Errorf("SanitizeProfile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureProfileStaysInsideRoot(t *testing.T) {
	work := t.TempDir()
	dir, err := EnsureProfile(work, "../../escape")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.Abs(RootDir(work))
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 2 && rel[:2] == ".." {
		t.Fatalf("profile dir %q escaped root %q (rel=%q)", dir, root, rel)
	}
}
