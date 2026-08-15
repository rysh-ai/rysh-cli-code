// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRyshBaseDirsRefusesHomeWhenCwdDeleted is the regression guard for a real
// incident: a daemon was started inside a git worktree, the worktree was then
// removed while it ran, and from that moment every .rysh lookup resolved
// against the user's $HOME/.rysh — reads and writes both — because a missing
// cwd and a cwd without .rysh were indistinguishable to os.Stat(".rysh").
// The user's global rysh state was destroyed. A dead working directory must
// never widen a process's reach.
func TestRyshBaseDirsRefusesHomeWhenCwdDeleted(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	dead := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dead) // restored to the original dir at cleanup
	if err := os.RemoveAll(dead); err != nil {
		t.Fatal(err)
	}
	dirs := ryshBaseDirs()
	for _, d := range dirs {
		if strings.HasPrefix(d, home) {
			t.Fatalf("a deleted working directory resolved to the user's global state: %q (all: %v)", d, dirs)
		}
	}
	if len(dirs) != 1 || dirs[0] != ".rysh" {
		t.Errorf("want only the local base so opens fail locally, got %v", dirs)
	}
}

// TestRyshBaseDirsKeepsHomeFallbackWhenCwdIsLive pins the other half: the
// $HOME fallback is how global skills and secrets are found, and the fix above
// must not cost that. A live directory with no .rysh still reaches home.
func TestRyshBaseDirsKeepsHomeFallbackWhenCwdIsLive(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	t.Chdir(t.TempDir()) // exists, but has no .rysh

	dirs := ryshBaseDirs()
	want := filepath.Join(home, ".rysh")
	for _, d := range dirs {
		if d == want {
			return
		}
	}
	t.Errorf("live cwd without .rysh must still fall back to %q, got %v", want, dirs)
}

// TestRyshBaseDirsPrefersProjectLocal keeps the ordering contract: a project's
// own .rysh outranks the user's.
func TestRyshBaseDirsPrefersProjectLocal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".rysh"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	dirs := ryshBaseDirs()
	if len(dirs) == 0 || dirs[0] != ".rysh" {
		t.Errorf("project-local .rysh must rank first, got %v", dirs)
	}
}

// TestDeadCwdCannotReachHomeSecrets pins the specific path that caused real
// damage. storeKind.readDirs() resolves through ryshBaseDirs, so a daemon whose
// worktree was deleted did not merely lose its project-local secrets — it
// adopted the user's ~/.rysh/secrets as though they were the project's.
func TestDeadCwdCannotReachHomeSecrets(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	dead := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dead)
	if err := os.RemoveAll(dead); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []storeKind{secretKind, variableKind} {
		for _, d := range kind.readDirs() {
			if strings.HasPrefix(d, home) {
				t.Errorf("%s lookup from a deleted cwd reached the user's home: %q", kind.label, d)
			}
		}
	}
}
