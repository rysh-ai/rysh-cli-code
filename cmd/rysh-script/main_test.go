package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRyshBinaryNames_ExcludesTheShim pins the bug this shim shipped with for
// exactly one build: the target name was derived with progname.Rewrite("rysh"),
// which resolves from argv[0] — this binary's own. It returned "rysh-script",
// so the shim found itself beside itself and exec'd itself, forever.
//
// A name list that can contain this binary's own name is a fork bomb, so it is
// checked rather than trusted.
func TestRyshBinaryNames_ExcludesTheShim(t *testing.T) {
	for _, n := range ryshBinaryNames {
		if n == "rysh-script" {
			t.Errorf("ryshBinaryNames contains %q — the shim would exec itself", n)
		}
	}
	if len(ryshBinaryNames) == 0 {
		t.Fatal("ryshBinaryNames is empty; the shim can never find rysh")
	}
}

// TestFindRysh_RefusesItself is the belt to that braces: even if a build were
// installed as one of the target names, findRysh must not return the running
// binary.
func TestFindRysh_RefusesItself(t *testing.T) {
	dir := t.TempDir()

	// Plant a file named like the real binary that IS a symlink to this test
	// binary, which is what os.Executable() reports.
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot resolve the test binary path")
	}
	link := filepath.Join(dir, "rysh")
	if err := os.Symlink(self, link); err != nil {
		t.Skipf("cannot symlink in this environment: %v", err)
	}

	got, err := findRysh()
	if err != nil {
		return // nothing found at all is a safe answer
	}
	resolvedGot, _ := filepath.EvalSymlinks(got)
	resolvedSelf, _ := filepath.EvalSymlinks(self)
	if resolvedGot != "" && resolvedGot == resolvedSelf {
		t.Errorf("findRysh returned the running binary (%s) — this would exec forever", got)
	}
}
