// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"context"
	"path/filepath"
	"testing"
)

// qa coverage proposal 3 (2026-11-01): a checkpoint outlives the process that
// saved it. The daemon keeps no checkpoint state in memory — each Store is a
// pane id over refs in the work tree's own repo — so a restarted daemon (here:
// the first Store dropped, a new one opened, from a subdirectory as a pane's
// shell may be) must list the same checkpoints and restore the same content.
func TestCheckpointSurvivesARestartAndRestoresEqualContent(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()

	first := open(t, dir, "pane-1")
	saved, created, err := first.Save(ctx, "before turn")
	if err != nil || !created {
		t.Fatalf("Save: created=%v err=%v", created, err)
	}
	writeT(t, dir, "a.txt", "agent edit\n")
	writeT(t, dir, "sub/new.txt", "added by the turn\n")
	// The daemon that saved it is gone; a new one opens the same pane.

	second := open(t, filepath.Join(dir, "sub"), "pane-1")
	if second.Root() != workTreeTop(t, dir) {
		t.Fatalf("reopened at %q, want the work tree top %q", second.Root(), workTreeTop(t, dir))
	}
	list, err := second.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Commit != saved.Commit || list[0].Label != "before turn" {
		t.Fatalf("after restart List = %+v, want the one checkpoint %s", list, saved.Commit)
	}

	if _, _, err := second.Restore(ctx, 1); err != nil {
		t.Fatalf("Restore after restart: %v", err)
	}
	if got := readT(t, dir, "a.txt"); got != "a1\n" {
		t.Errorf("a.txt = %q, want the checkpointed a1", got)
	}
	if got := readT(t, dir, "sub/b.txt"); got != "b1\n" {
		t.Errorf("sub/b.txt = %q, want b1", got)
	}
	if exists(dir, "sub/new.txt") {
		t.Error("a file the turn added survived the restore")
	}

	// Another pane's store over the same tree sees none of it.
	if other, err := open(t, dir, "pane-2").List(ctx); err != nil || len(other) != 0 {
		t.Errorf("pane-2 List = %+v, %v; checkpoints must stay per pane", other, err)
	}
}

// workTreeTop is the work tree's top level as git reports it (it resolves a
// symlinked TempDir the same way Open does).
func workTreeTop(t *testing.T, dir string) string {
	t.Helper()
	return gitT(t, dir, "rev-parse", "--show-toplevel")
}
