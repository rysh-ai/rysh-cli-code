// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeT(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readT(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func exists(dir, name string) bool {
	_, err := os.Lstat(filepath.Join(dir, name))
	return err == nil
}

// newRepo makes a repo with one commit: a.txt, sub/b.txt and a .gitignore
// that ignores *.log.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.email", "t@example.com")
	gitT(t, dir, "config", "user.name", "T")
	writeT(t, dir, "a.txt", "a1\n")
	writeT(t, dir, "sub/b.txt", "b1\n")
	writeT(t, dir, ".gitignore", "*.log\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// userState is everything of the user's that a checkpoint must not touch.
type userState struct {
	head, branch, staged, stash string
	index                       []byte
}

func snapUser(t *testing.T, dir string) userState {
	t.Helper()
	idx, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return userState{
		head:   gitT(t, dir, "rev-parse", "HEAD"),
		branch: gitT(t, dir, "symbolic-ref", "HEAD"),
		staged: gitT(t, dir, "diff", "--cached", "--name-status"),
		stash:  gitT(t, dir, "stash", "list"),
		index:  idx,
	}
}

func assertUserUntouched(t *testing.T, dir string, before userState) {
	t.Helper()
	after := snapUser(t, dir)
	if after.head != before.head || after.branch != before.branch {
		t.Fatalf("HEAD/branch moved: %s %s -> %s %s", before.branch, before.head, after.branch, after.head)
	}
	if after.staged != before.staged {
		t.Fatalf("staged set changed: %q -> %q", before.staged, after.staged)
	}
	if after.stash != before.stash {
		t.Fatalf("stash changed: %q -> %q", before.stash, after.stash)
	}
	if !bytes.Equal(after.index, before.index) {
		t.Fatal("the user's .git/index was rewritten")
	}
}

func open(t *testing.T, dir, pane string) *Store {
	t.Helper()
	s, err := Open(dir, pane)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestSaveNeverTouchesUserState(t *testing.T) {
	dir := newRepo(t)
	writeT(t, dir, "a.txt", "a2\n")
	gitT(t, dir, "add", "a.txt") // the user has something staged
	writeT(t, dir, "new.txt", "n\n")
	before := snapUser(t, dir)

	s := open(t, dir, "pane-1")
	cp, created, err := s.Save(context.Background(), "turn 1")
	if err != nil || !created {
		t.Fatalf("Save: created=%v err=%v", created, err)
	}
	if cp.N != 1 || cp.Label != "turn 1" || cp.Commit == "" {
		t.Fatalf("bad checkpoint %+v", cp)
	}
	assertUserUntouched(t, dir, before)
	if refs := gitT(t, dir, "for-each-ref", "--format=%(refname)", "refs/rysh/checkpoints/pane-1/"); refs == "" {
		t.Fatal("no shadow ref written")
	}
	if br := gitT(t, dir, "branch", "--list"); strings.Count(br, "\n") != 0 {
		t.Fatalf("a branch was created: %q", br)
	}
}

func TestSaveDedupesUnchangedTree(t *testing.T) {
	dir := newRepo(t)
	s := open(t, dir, "p")
	ctx := context.Background()
	if _, created, err := s.Save(ctx, "one"); err != nil || !created {
		t.Fatalf("first save: %v %v", created, err)
	}
	cp, created, err := s.Save(ctx, "two")
	if err != nil || created {
		t.Fatalf("second save of the same tree must not create: created=%v err=%v", created, err)
	}
	if cp.Label != "one" {
		t.Fatalf("dedupe should return the existing newest, got %q", cp.Label)
	}
	writeT(t, dir, "a.txt", "changed\n")
	if _, created, err := s.Save(ctx, "three"); err != nil || !created {
		t.Fatalf("changed tree must create: %v %v", created, err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 2 || list[0].Label != "three" || list[0].N != 1 || list[1].N != 2 {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func TestRestoreBringsBackFilesAndKeepsIgnored(t *testing.T) {
	dir := newRepo(t)
	writeT(t, dir, "untracked-kept.txt", "u1\n")
	writeT(t, dir, "debug.log", "ignored-1\n")
	s := open(t, dir, "p")
	ctx := context.Background()
	if _, _, err := s.Save(ctx, "before turn"); err != nil {
		t.Fatal(err)
	}

	// The agent's turn: edit, delete, add, nested add, edit untracked, edit ignored.
	writeT(t, dir, "a.txt", "agent edit\n")
	if err := os.Remove(filepath.Join(dir, "sub/b.txt")); err != nil {
		t.Fatal(err)
	}
	writeT(t, dir, "agent-new.txt", "x\n")
	writeT(t, dir, "deep/er/c.txt", "y\n")
	writeT(t, dir, "untracked-kept.txt", "u2\n")
	writeT(t, dir, "debug.log", "ignored-2\n")
	before := snapUser(t, dir)

	restored, safety, err := s.Restore(ctx, 1)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.Label != "before turn" || safety.Commit == "" {
		t.Fatalf("restored=%+v safety=%+v", restored, safety)
	}
	if got := readT(t, dir, "a.txt"); got != "a1\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := readT(t, dir, "sub/b.txt"); got != "b1\n" {
		t.Fatalf("sub/b.txt = %q", got)
	}
	if got := readT(t, dir, "untracked-kept.txt"); got != "u1\n" {
		t.Fatalf("untracked = %q", got)
	}
	if exists(dir, "agent-new.txt") || exists(dir, "deep/er/c.txt") || exists(dir, "deep") {
		t.Fatal("files the turn added must be removed, with their empty dirs")
	}
	if got := readT(t, dir, "debug.log"); got != "ignored-2\n" {
		t.Fatalf("an ignored file must never be touched, got %q", got)
	}
	assertUserUntouched(t, dir, before)
}

func TestRestoreIsItselfUndoable(t *testing.T) {
	dir := newRepo(t)
	s := open(t, dir, "p")
	ctx := context.Background()
	if _, _, err := s.Save(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	writeT(t, dir, "a.txt", "turn output\n")
	writeT(t, dir, "made.txt", "m\n")
	_, safety, err := s.Restore(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx)
	if len(list) != 2 || list[0].Commit != safety.Commit || !IsSafety(list[0]) {
		t.Fatalf("the safety checkpoint must be the newest: %+v", list)
	}
	if _, _, err := s.Restore(ctx, 1); err != nil { // undo the undo
		t.Fatal(err)
	}
	if got := readT(t, dir, "a.txt"); got != "turn output\n" || !exists(dir, "made.txt") {
		t.Fatalf("redo failed: a.txt=%q made=%v", got, exists(dir, "made.txt"))
	}
}

func TestRestoreOutOfRange(t *testing.T) {
	dir := newRepo(t)
	s := open(t, dir, "p")
	if _, _, err := s.Restore(context.Background(), 1); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("restore with none: %v", err)
	}
	if _, _, err := s.Save(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, -1, 2} {
		if _, _, err := s.Restore(context.Background(), n); !errors.Is(err, ErrNoCheckpoint) {
			t.Fatalf("restore %d: %v", n, err)
		}
	}
}

func TestOpenRefusesNonRepoAndBadPane(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if _, err := Open(t.TempDir(), "p"); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("non-repo: %v", err)
	}
	dir := newRepo(t)
	for _, bad := range []string{"", "../x", "a/b", "a b", "a..b", "-x", strings.Repeat("a", 200)} {
		if _, err := Open(dir, bad); err == nil {
			t.Fatalf("pane %q must be refused", bad)
		}
	}
	// A subdirectory resolves to the work-tree top level.
	s := open(t, dir, "p")
	sub := open(t, filepath.Join(dir, "sub"), "p")
	if s.Root() != sub.Root() {
		t.Fatalf("roots differ: %s vs %s", s.Root(), sub.Root())
	}
}

func TestPanesAreSeparate(t *testing.T) {
	dir := newRepo(t)
	ctx := context.Background()
	a, b := open(t, dir, "pa"), open(t, dir, "pb")
	if _, _, err := a.Save(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if l, _ := b.List(ctx); len(l) != 0 {
		t.Fatalf("pane b sees pane a's checkpoints: %+v", l)
	}
}

func TestUnbornRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	writeT(t, dir, "f.txt", "1\n")
	s := open(t, dir, "p")
	ctx := context.Background()
	if _, _, err := s.Save(ctx, "first"); err != nil {
		t.Fatalf("save in unborn repo: %v", err)
	}
	writeT(t, dir, "f.txt", "2\n")
	if _, _, err := s.Restore(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := readT(t, dir, "f.txt"); got != "1\n" {
		t.Fatalf("f.txt = %q", got)
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	dir := newRepo(t)
	s := open(t, dir, "p")
	s.Keep = 3
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		writeT(t, dir, "a.txt", strings.Repeat("x", i+1))
		if _, _, err := s.Save(ctx, "t"+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.List(ctx)
	if err != nil || len(l) != 3 || l[0].Label != "t4" || l[2].Label != "t2" {
		t.Fatalf("List after prune = %+v, %v", l, err)
	}
}

func TestSizeGuardSkips(t *testing.T) {
	dir := newRepo(t)
	s := open(t, dir, "p")
	s.MaxNewBytes = 10
	writeT(t, dir, "big.bin", strings.Repeat("z", 100))
	if _, _, err := s.Save(context.Background(), "big"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if l, _ := s.List(context.Background()); len(l) != 0 {
		t.Fatal("nothing may be stored when the guard trips")
	}
}

// A repo's own config must not make the daemon run code: no hooks, no
// clean filters, and no inherited GIT_DIR redirect.
func TestRepoConfigCannotRunCode(t *testing.T) {
	dir := newRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	gitT(t, dir, "config", "filter.evil.clean", "echo filter >> "+marker+"; cat")
	gitT(t, dir, "config", "filter.evil.required", "true")
	writeT(t, dir, ".gitattributes", "*.txt filter=evil\n")
	writeT(t, dir, ".git/hooks/post-commit", "#!/bin/sh\necho hook >> "+marker+"\n")
	if err := os.Chmod(filepath.Join(dir, ".git/hooks/post-commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	s := open(t, dir, "p")
	writeT(t, dir, "a.txt", "edited\n")
	if _, _, err := s.Save(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		b, _ := os.ReadFile(marker)
		t.Fatalf("a repo filter or hook ran during a checkpoint: %s", b)
	}
}

func TestDiffNamesChangedFiles(t *testing.T) {
	dir := newRepo(t)
	s := open(t, dir, "p")
	ctx := context.Background()
	if _, _, err := s.Save(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	writeT(t, dir, "a.txt", "changed\n")
	writeT(t, dir, "added.txt", "n\n")
	out, err := s.Diff(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "added.txt") || strings.Contains(out, "sub/b.txt") {
		t.Fatalf("Diff = %q", out)
	}
}

// Undo walks back one turn at a time: it skips the safety checkpoints that
// restores leave behind, and checkpoints equal to the tree as it is now.
func TestUndoStepsBackTurnByTurn(t *testing.T) {
	dir := newRepo(t)
	s := open(t, dir, "p")
	ctx := context.Background()
	for i, body := range []string{"t1\n", "t2\n", "t3\n"} {
		writeT(t, dir, "a.txt", body)
		if _, _, err := s.Save(ctx, "turn "+string(rune('1'+i))); err != nil {
			t.Fatal(err)
		}
	}
	writeT(t, dir, "a.txt", "t4\n") // the last turn's edit, not yet checkpointed
	for _, want := range []string{"t3\n", "t2\n", "t1\n"} {
		if _, _, err := s.Undo(ctx); err != nil {
			t.Fatalf("Undo to %q: %v", want, err)
		}
		if got := readT(t, dir, "a.txt"); got != want {
			t.Fatalf("after undo a.txt = %q, want %q", got, want)
		}
	}
	if _, _, err := s.Undo(ctx); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("undo past the oldest: %v", err)
	}
	// The newest safety checkpoint still holds the state before the last undo.
	l, _ := s.List(ctx)
	if !IsSafety(l[0]) {
		t.Fatalf("newest is not a safety checkpoint: %+v", l[0])
	}
}

// A same-size rewrite in the tick the user's index was written must still be
// checkpointed. The snapshot seeds its index from a copy of the user's; git
// re-hashes "racy" entries (mtime not older than the index file), so the copy
// must keep the source's mtime or git trusts a stale stat and keeps the old
// blob (seen under load: TestUndoStepsBackTurnByTurn in the whole suite).
func TestSaveSeesASameSizeRewriteInTheIndexTick(t *testing.T) {
	dir := newRepo(t)
	gitT(t, dir, "config", "core.trustctime", "false") // only mtime/size/inode decide
	past := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	a := filepath.Join(dir, "a.txt")
	if err := os.Chtimes(a, past, past); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "update-index", "--refresh") // the entry now records mtime == past
	writeT(t, dir, "a.txt", "zz\n")           // same size as "a1\n"
	if err := os.Chtimes(a, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, ".git", "index"), past, past); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir, "p")
	ctx := context.Background()
	if _, _, err := s.Save(ctx, "turn 1"); err != nil {
		t.Fatal(err)
	}
	writeT(t, dir, "a.txt", "later\n")
	if _, _, err := s.Undo(ctx); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if got := readT(t, dir, "a.txt"); got != "zz\n" {
		t.Fatalf("checkpoint missed the same-size rewrite: a.txt = %q, want %q", got, "zz\n")
	}
}

// Restore never deletes through a symlink: a directory swapped for a link
// between the snapshot and the delete would point the delete outside the
// repository (lead review, minor).
func TestRemoveInRepoRefusesSymlinkedParents(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeT(t, outside, "x", "keep me")
	if err := os.Symlink(outside, filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	if err := removeInRepo(root, "d/x"); !errors.Is(err, errOutsideRepo) {
		t.Fatalf("removeInRepo through a symlinked dir: %v", err)
	}
	if readT(t, outside, "x") != "keep me" {
		t.Fatal("a file outside the repository was touched")
	}
	writeT(t, root, "e/f/y", "go")
	if err := removeInRepo(root, "e/f/y"); err != nil || exists(root, "e/f/y") {
		t.Fatalf("plain remove: %v", err)
	}
	// A final symlink is removed itself, never its target.
	if err := os.Symlink(filepath.Join(outside, "x"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := removeInRepo(root, "link"); err != nil || readT(t, outside, "x") != "keep me" {
		t.Fatalf("final symlink: %v", err)
	}
	if err := removeInRepo(root, "../x"); !errors.Is(err, errOutsideRepo) {
		t.Fatalf("an escaping path: %v", err)
	}
}
