// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
)

// The empty-board hint must name a binary the reader can actually run.
//
// It said the literal `rysh` until 2026-08-14, and there is no `rysh` on the
// machine this ships from — `make build` emits `rysh_local`, README promises
// `./rysh`, the root Makefile echoes `ry` (design 025 §8d, three names in one
// install path). The hint is read by an agent whose post just failed, so a
// wrong name here is the instruction someone follows while already lost. Two
// real claudes did exactly that in an isolated session: `bash: rysh: command
// not found`, exit 127, an empty board, and ABLA/ANSA/the store/this view all
// working the whole time.
//
// The rule these tests pin is the one the rest of this stack uses: PROVE THE
// SHORT NAME, never guess it. A base name is claimed only when LookPath finds
// it AND it resolves to this same file; otherwise the hint carries the absolute
// path, which is longer and correct from any cwd — including the worktree an
// agent actually runs in.

func TestEmptyBoardHintNamesARunnableBinary(t *testing.T) {
	m := buildBoardModel(board.New(0))
	m.boardRecorder = RecorderLive

	rows := strings.Join(m.boardRows("", 100), "\n")
	if !strings.Contains(rows, "Nothing posted yet") {
		t.Fatalf("precondition: expected the empty-board hint:\n%s", rows)
	}

	bin := boardHintBinary()
	if !strings.Contains(rows, bin+" board post") {
		t.Errorf("the hint must name this binary (%q):\n%s", bin, rows)
	}

	// The regression itself. `rysh` may legitimately BE the name (a machine
	// where the binary installs under it), so this asserts the hint is not the
	// hardcoded literal while the running binary is called something else.
	self, err := os.Executable()
	if err == nil && filepath.Base(self) != "rysh" {
		if strings.Contains(rows, "`rysh board post") {
			t.Errorf("the hint still hardcodes `rysh` while this binary is %q — "+
				"that is the exit-127 defect:\n%s", filepath.Base(self), rows)
		}
	}
}

func TestBoardHintBinaryIsResolvableFromAnyCwd(t *testing.T) {
	// An agent reads the brief and the board from its own worktree, never from
	// the workspace root. Whatever the hint names has to work there, so it is
	// either an absolute path or a name that is genuinely on PATH.
	bin := boardHintBinary()
	if filepath.IsAbs(bin) {
		if _, err := os.Stat(bin); err != nil {
			t.Errorf("hint names absolute %q which does not exist: %v", bin, err)
		}
		return
	}
	if strings.ContainsRune(bin, filepath.Separator) {
		t.Errorf("hint names a RELATIVE path %q — it resolves differently in "+
			"every agent's worktree, which is the same defect one level down", bin)
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Errorf("hint names bare %q, which is not on PATH: %v — a short name "+
			"must be proved before it is claimed", bin, err)
	}
}

func TestBoardHintBinaryPrefersTheShortNameOnlyWhenItIsThisFile(t *testing.T) {
	// The half that keeps the optimisation honest: `foo` being on PATH is not
	// evidence when the PATH `foo` is a different program that happens to share
	// the name. Same name AND same file, or the absolute path.
	bin := boardHintBinary()
	if filepath.IsAbs(bin) {
		return // took the safe branch; nothing to check
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		t.Fatalf("short name %q was claimed but does not resolve: %v", bin, err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable unavailable on this platform")
	}
	a, err1 := filepath.EvalSymlinks(resolved)
	b, err2 := filepath.EvalSymlinks(self)
	if err1 != nil || err2 != nil {
		t.Fatalf("could not compare %q and %q: %v / %v", resolved, self, err1, err2)
	}
	if a != b {
		t.Errorf("hint claims %q, but PATH resolves it to %q while this binary "+
			"is %q — a different program answering to the same name", bin, a, b)
	}
}
