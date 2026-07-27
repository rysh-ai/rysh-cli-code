package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Design 008, pane-side gaps: `##pane new --worktree [branch]` and worktree
// cleanup-on-close. Mirrors the harness style of worktree_cwd_test.go /
// agent_worktree_isolation_test.go: temp git repos, a WorkspaceActor built by
// hand, pub left nil so reaching a NATS send where none is expected panics —
// which is itself the regression signal.

// --- ##pane new --worktree -------------------------------------------------

// TestProvisionPaneWorktreeCreatesAndReuses: the default branch is
// pane/<alias> under .rysh/worktrees (mirroring agent/<name>), a second pane
// on the same branch reuses the worktree, and an explicit branch argument
// overrides the default.
func TestProvisionPaneWorktreeCreatesAndReuses(t *testing.T) {
	root, _ := gitRepoWithWorktree(t, "unrelated")
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}}

	var out strings.Builder
	branch, path := w.provisionPaneWorktree(&out, root, "", "gentle-otter")
	if branch != "pane/gentle-otter" {
		t.Fatalf("branch = %q, want pane/gentle-otter", branch)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("worktree dir not created at %s: %v", path, err)
	}
	if !strings.Contains(path, filepath.Join(".rysh", "worktrees")) {
		t.Fatalf("worktree not under .rysh/worktrees: %s", path)
	}
	if !strings.Contains(out.String(), "created worktree") {
		t.Fatalf("output missing creation report: %s", out.String())
	}

	out.Reset()
	branch2, path2 := w.provisionPaneWorktree(&out, root, "", "gentle-otter")
	if branch2 != branch || path2 != path {
		t.Fatalf("re-provision changed the worktree: (%q,%q) vs (%q,%q)", branch2, path2, branch, path)
	}
	if !strings.Contains(out.String(), "reusing worktree") {
		t.Fatalf("re-provision must reuse, got: %s", out.String())
	}

	out.Reset()
	branch3, _ := w.provisionPaneWorktree(&out, root, "feat/my-thing", "ignored-alias")
	if branch3 != "feat/my-thing" {
		t.Fatalf("explicit branch arg not honoured: %q", branch3)
	}
}

// TestPaneNewWorktreeFailsClosedOutsideGit: unlike agent spawn (which degrades
// to the shared checkout), `##pane new --worktree` outside a git repo must
// create NO pane. pub is nil, so reaching the pane-create send would panic —
// returning cleanly with an error report is the assertion.
func TestPaneNewWorktreeFailsClosedOutsideGit(t *testing.T) {
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: t.TempDir()}}
	var out strings.Builder
	w.handlePaneNewCommand(nil, &out, "pane-1", []string{"--worktree"})
	if !strings.Contains(out.String(), "not a git repository") {
		t.Fatalf("expected a loud non-git refusal, got: %s", out.String())
	}
	if len(w.groupWorktrees) != 0 {
		t.Fatalf("no group must be tracked on failure, got %v", w.groupWorktrees)
	}
}

// --- cleanup-on-close ------------------------------------------------------

// TestReleaseGroupWorktreeRemovesClean: the last user of a CLEAN worktree
// closing removes the worktree directory and reports it.
func TestReleaseGroupWorktreeRemovesClean(t *testing.T) {
	root, wtPath := gitRepoWithWorktree(t, "pane/clean-close")
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}}
	w.trackGroupWorktree("group-1", wtPath, "pane/clean-close")

	report := w.releaseGroupWorktree("group-1")
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("clean worktree must be removed on close, still at %s (report: %s)", wtPath, report)
	}
	if !strings.Contains(report, "worktree removed") {
		t.Fatalf("removal must be reported, got: %q", report)
	}
	if len(w.groupWorktrees) != 0 {
		t.Fatalf("tracking record must be dropped, got %v", w.groupWorktrees)
	}
}

// TestReleaseGroupWorktreeKeepsDirty: uncommitted work is NEVER discarded —
// the worktree is kept and the report carries the path plus `git diff --stat`
// so the user knows where the work survives.
func TestReleaseGroupWorktreeKeepsDirty(t *testing.T) {
	root, wtPath := gitRepoWithWorktree(t, "pane/dirty-close")
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}}
	w.trackGroupWorktree("group-1", wtPath, "pane/dirty-close")

	// Dirty the worktree by modifying a TRACKED file (diff --stat is empty for
	// untracked files, and the report must show real stat output).
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("uncommitted agent work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report := w.releaseGroupWorktree("group-1")
	if info, err := os.Stat(wtPath); err != nil || !info.IsDir() {
		t.Fatalf("dirty worktree must be KEPT, gone from %s: %v", wtPath, err)
	}
	if !strings.Contains(report, "DIRTY") || !strings.Contains(report, wtPath) {
		t.Fatalf("report must flag DIRTY and include the path, got: %q", report)
	}
	if !strings.Contains(report, "README.md") {
		t.Fatalf("report must include `git diff --stat` output, got: %q", report)
	}
}

// TestReleaseGroupWorktreeKeepsWhenShared: a worktree still used by another
// pane group must NOT be removed; it is removed only when the LAST user closes.
func TestReleaseGroupWorktreeKeepsWhenShared(t *testing.T) {
	root, wtPath := gitRepoWithWorktree(t, "pane/shared")
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}}
	w.trackGroupWorktree("group-1", wtPath, "pane/shared")
	w.trackGroupWorktree("group-2", wtPath, "pane/shared")

	report := w.releaseGroupWorktree("group-1")
	if info, err := os.Stat(wtPath); err != nil || !info.IsDir() {
		t.Fatalf("worktree with a surviving user must be kept, gone from %s", wtPath)
	}
	if !strings.Contains(report, "still used") {
		t.Fatalf("report must say the worktree is still in use, got: %q", report)
	}

	// Last user closes -> now it is removed.
	report = w.releaseGroupWorktree("group-2")
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("worktree must be removed when the last user closes (report: %s)", report)
	}
}

// TestReleaseGroupWorktreeUntrackedIsNoop: groups that never ran in a
// rysh-managed worktree release nothing — and after a restart (tracking is
// in-memory) the fail-safe is exactly this no-op, never a removal.
func TestReleaseGroupWorktreeUntrackedIsNoop(t *testing.T) {
	root, wtPath := gitRepoWithWorktree(t, "pane/untracked")
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}}

	if report := w.releaseGroupWorktree("never-tracked"); report != "" {
		t.Fatalf("untracked release must be silent, got: %q", report)
	}
	if info, err := os.Stat(wtPath); err != nil || !info.IsDir() {
		t.Fatalf("untracked worktree must be untouched, gone from %s", wtPath)
	}
}

// TestGroupClosedByPaneClose: the keyboard-close hook must mirror
// TabActor.closePaneInLane — release the group's worktree exactly when the
// close tears down the whole group (or its lane), never for a stacked pane.
func TestGroupClosedByPaneClose(t *testing.T) {
	pane := func(id string) domain.PaneSnapshot { return domain.PaneSnapshot{ID: id} }
	group := func(id string, panes ...domain.PaneSnapshot) domain.PaneGroupSnapshot {
		return domain.PaneGroupSnapshot{ID: id, Panes: panes}
	}
	lane := func(id string, groups ...domain.PaneGroupSnapshot) domain.LaneSnapshot {
		return domain.LaneSnapshot{ID: id, PaneGroups: groups}
	}

	cases := []struct {
		name string
		snap *domain.TabSnapshot
		want string
	}{
		{"nil snapshot", nil, ""},
		{"stacked pane closes, group survives", &domain.TabSnapshot{
			ActivePaneID: "p1",
			Lanes:        []domain.LaneSnapshot{lane("l1", group("g1", pane("p1"), pane("p2")))},
		}, ""},
		{"single-pane group among siblings closes", &domain.TabSnapshot{
			ActivePaneID: "p1",
			Lanes:        []domain.LaneSnapshot{lane("l1", group("g1", pane("p1")), group("g2", pane("p2")))},
		}, "g1"},
		{"lane with one group closes (multi-lane tab)", &domain.TabSnapshot{
			ActivePaneID: "p1",
			Lanes: []domain.LaneSnapshot{
				lane("l1", group("g1", pane("p1"))),
				lane("l2", group("g2", pane("p2"))),
			},
		}, "g1"},
		{"last group of last lane: close is a no-op", &domain.TabSnapshot{
			ActivePaneID: "p1",
			Lanes:        []domain.LaneSnapshot{lane("l1", group("g1", pane("p1")))},
		}, ""},
	}
	for _, tc := range cases {
		if got := groupClosedByPaneClose(tc.snap); got != tc.want {
			t.Errorf("%s: groupClosedByPaneClose = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestWorktreeCwdTracksGroupForCleanup would require a pub; instead pin the
// tracking primitive the retarget sites call: a tracked group is exactly one
// release away from cleanup, and tracking "" never records garbage.
func TestTrackGroupWorktreePrimitive(t *testing.T) {
	w := &WorkspaceActor{}
	w.trackGroupWorktree("", "/x", "b")
	w.trackGroupWorktree("g", "", "b")
	if len(w.groupWorktrees) != 0 {
		t.Fatalf("empty ids must not be tracked: %v", w.groupWorktrees)
	}
	w.trackGroupWorktree("g", "/x", "b")
	if ref := w.groupWorktrees["g"]; ref.path != "/x" || ref.branch != "b" {
		t.Fatalf("tracked ref = %+v", ref)
	}
}

// TestPaneGroupCfgOverridesWorkingDir: the birth-time cwd override — the
// mechanism that makes `##pane new --worktree` race-free (the group's first
// pane starts in the worktree, no follow-up MsgSetWorkingDir needed).
func TestPaneGroupCfgOverridesWorkingDir(t *testing.T) {
	base := config.Config{WorkingDirectory: "/main/checkout"}
	if got := paneGroupCfg(base, "/repo/.rysh/worktrees/pane-x").WorkingDirectory; got != "/repo/.rysh/worktrees/pane-x" {
		t.Fatalf("override not applied: %q", got)
	}
	if got := paneGroupCfg(base, "").WorkingDirectory; got != "/main/checkout" {
		t.Fatalf("empty override must keep the lane cwd: %q", got)
	}
}

// TestHelpMentionsPaneNewWorktree: the trigger must be discoverable — ##help
// and the ##pane usage block are how users find it.
func TestHelpMentionsPaneNewWorktree(t *testing.T) {
	w := &WorkspaceActor{}
	var out strings.Builder
	w.ryshHelp(&out)
	if !strings.Contains(out.String(), "##pane new [--worktree [branch]]") {
		t.Fatalf("##help does not advertise ##pane new --worktree")
	}
}
