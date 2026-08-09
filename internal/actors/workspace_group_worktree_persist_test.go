package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/worktree"
)

// Design 008, E-17 first half: pane-group worktree OWNERSHIP must survive a
// daemon restart. groupWorktrees was in-memory only, so after a restart the
// close of a pane group that owned a rysh-provisioned worktree found no
// record, returned "", and orphaned the worktree — the user had to find it and
// `##worktree remove` it by hand. The agent half of design 008 already
// round-trips its worktree record through KV (agent_registry.go); this is the
// pane-group equivalent.
//
// Harness style follows pane_worktree_lifecycle_test.go (temp git repos, a
// WorkspaceActor built by hand, pub left nil) plus the recordingKV fixture
// from humanoid_registry_persist_test.go, which is what a real restart reads
// back: the exact bytes the previous process wrote.

// restartWorkspace simulates the daemon restart: a brand-new WorkspaceActor
// with an empty map, over the SAME KV store, running the same restore the
// Started hook runs.
func restartWorkspace(root string, kv *recordingKV) *WorkspaceActor {
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}, wKV: kv}
	w.restoreGroupWorktreesFromKV()
	return w
}

// TestGroupWorktreeOwnershipSurvivesRestart is the regression test for E-17:
// track → restart → the record is back, and closing the last owning group
// still cleans up the clean worktree instead of orphaning it.
func TestGroupWorktreeOwnershipSurvivesRestart(t *testing.T) {
	root, wtPath := gitRepoWithWorktree(t, "pane/restart-clean")
	kv := &recordingKV{}

	before := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}, wKV: kv}
	before.trackGroupWorktree("group-1", wtPath, "pane/restart-clean")

	after := restartWorkspace(root, kv)
	ref, ok := after.groupWorktrees["group-1"]
	if !ok {
		t.Fatalf("ownership record lost across restart: a post-restart close cannot clean up (map: %v)", after.groupWorktrees)
	}
	if ref.path != wtPath || ref.branch != "pane/restart-clean" {
		t.Fatalf("restored record = %+v, want path %q branch %q", ref, wtPath, "pane/restart-clean")
	}

	report := after.releaseGroupWorktree("group-1")
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("clean worktree must be removed on a post-restart close, still at %s (report: %q)", wtPath, report)
	}
	if !strings.Contains(report, "worktree removed") {
		t.Fatalf("removal must be reported after a restart, got: %q", report)
	}
	// The release must also be durable: a second restart must not resurrect
	// the record for a worktree that is already gone.
	if again := restartWorkspace(root, kv); len(again.groupWorktrees) != 0 {
		t.Fatalf("released record survived into the next restart: %v", again.groupWorktrees)
	}
}

// TestRestoredGroupWorktreeStillKeepsDirty: restoring a record must never make
// a removal the in-memory path would not have made. Dirty stays kept.
func TestRestoredGroupWorktreeStillKeepsDirty(t *testing.T) {
	root, wtPath := gitRepoWithWorktree(t, "pane/restart-dirty")
	kv := &recordingKV{}

	before := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}, wKV: kv}
	before.trackGroupWorktree("group-1", wtPath, "pane/restart-dirty")

	// Uncommitted work in a TRACKED file (diff --stat is empty for untracked).
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("uncommitted agent work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	after := restartWorkspace(root, kv)
	report := after.releaseGroupWorktree("group-1")
	if info, err := os.Stat(wtPath); err != nil || !info.IsDir() {
		t.Fatalf("restored record must still KEEP a dirty worktree, gone from %s: %v", wtPath, err)
	}
	if !strings.Contains(report, "DIRTY") {
		t.Fatalf("dirty keep must be reported after a restart, got: %q", report)
	}
}

// TestDegradedKVRestoreFailsSafeToKeep pins the hard requirement of the
// ownership model (workspace_pane_worktree.go:20-32): "fail-safe is always
// keep". A KV read that fails, or returns garbage, restores NO records — and
// no record means the close path returns "" and touches nothing. It must never
// degrade towards a removal.
func TestDegradedKVRestoreFailsSafeToKeep(t *testing.T) {
	root, wtPath := gitRepoWithWorktree(t, "pane/degraded")

	cases := []struct {
		name  string
		store func() *recordingKV
	}{
		{"key missing (Get fails)", func() *recordingKV { return &recordingKV{} }},
		{"garbage bytes", func() *recordingKV {
			kv := &recordingKV{}
			_, _ = kv.Put((&WorkspaceActor{}).groupWorktreeKVKey(), []byte("{not json at all"))
			return kv
		}},
		{"well-formed JSON of the wrong shape", func() *recordingKV {
			kv := &recordingKV{}
			_, _ = kv.Put((&WorkspaceActor{}).groupWorktreeKVKey(), []byte(`["group-1","group-2"]`))
			return kv
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := restartWorkspace(root, tc.store())
			if len(w.groupWorktrees) != 0 {
				t.Fatalf("a degraded KV read must restore nothing, got %v", w.groupWorktrees)
			}
			if report := w.releaseGroupWorktree("group-1"); report != "" {
				t.Fatalf("untracked group must be a silent no-op, got: %q", report)
			}
			if info, err := os.Stat(wtPath); err != nil || !info.IsDir() {
				t.Fatalf("worktree must be KEPT when ownership is unknown, gone from %s: %v", wtPath, err)
			}
		})
	}
}

// TestRestoreDropsRecordForVanishedWorktree: a record whose directory no
// longer exists (manual rm -rf, crash) is pruned on restore — nothing is
// removed, and the pruning is written back so it does not come round again.
func TestRestoreDropsRecordForVanishedWorktree(t *testing.T) {
	root, wtPath := gitRepoWithWorktree(t, "pane/vanished")
	kv := &recordingKV{}

	before := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}, wKV: kv}
	before.trackGroupWorktree("group-1", wtPath, "pane/vanished")
	before.trackGroupWorktree("group-2", worktree.Dir(root, "pane/never-existed"), "pane/never-existed")

	// The directory disappears while the daemon is down.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatal(err)
	}

	after := restartWorkspace(root, kv)
	if len(after.groupWorktrees) != 0 {
		t.Fatalf("records for vanished worktrees must be pruned, got %v", after.groupWorktrees)
	}
	if next := restartWorkspace(root, kv); len(next.groupWorktrees) != 0 {
		t.Fatalf("pruning must be persisted, records came back: %v", next.groupWorktrees)
	}
	// The repo itself is untouched: pruning a record removes no directory.
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("pruning a stale record must not touch the repo: %v", err)
	}
}
