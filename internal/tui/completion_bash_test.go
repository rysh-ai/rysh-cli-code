// SPDX-License-Identifier: Apache-2.0

package tui

// Phase 4 (bash-shell-mode): bash programmable-completion sidecar tests.
// These exercise a real bash; they skip when bash or the completion spec is
// unavailable so CI stays green on minimal images.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBashCompletionGitSubcommands(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cands := bashCompletionCandidates(t.TempDir(), "git chec")
	if len(cands) == 0 {
		t.Skip("bash-completion not installed (no git spec) — skipping")
	}
	found := false
	for _, c := range cands {
		if c == "checkout" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("git chec<Tab> candidates %v do not include 'checkout'", cands)
	}
}

func TestBashCompletionUnknownCommand(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	if cands := bashCompletionCandidates(t.TempDir(), "definitely-not-a-command-xyz --fl"); len(cands) != 0 {
		t.Errorf("expected no candidates for unknown command, got %v", cands)
	}
}

func TestMarkDirCandidates(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := markDirCandidates([]string{"sub", "file.txt", "already/"}, dir)
	want := []string{"sub/", "file.txt", "already/"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("markDirCandidates[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
