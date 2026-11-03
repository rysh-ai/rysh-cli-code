// SPDX-License-Identifier: Apache-2.0

// Package checkpoint keeps per-pane snapshots of a git work tree so an agent's
// file edits can be undone (gap G7, out/gap09-design.md).
//
// A checkpoint is a commit whose tree is the whole work tree as `git add -A`
// would see it: tracked and untracked files, .gitignore respected. It is built
// in a temporary index and stored under a shadow ref,
//
//	refs/rysh/checkpoints/<pane>/<seq>
//
// so the user's HEAD, branch, index and stash are never read-modified-written.
// Restore writes a checkpoint's files back into the work tree (again through a
// temporary index) after first saving the current state, so every restore can
// itself be undone.
//
// Git runs hardened, as in peercoord's checkpointTask: no hooks, no fsmonitor,
// no signing, every configured clean/process filter neutralised, and no GIT_*
// variable inherited from the daemon's environment.
package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	refPrefix = "refs/rysh/checkpoints/"
	// DefaultKeep is how many checkpoints a pane keeps; older ones are pruned.
	DefaultKeep = 50
	// DefaultMaxNewBytes bounds the untracked/modified content one checkpoint
	// may add (design note QUESTION 3).
	DefaultMaxNewBytes int64 = 100 << 20
)

var (
	// ErrNotRepo: the directory is not inside a git work tree.
	ErrNotRepo = errors.New("not inside a git work tree")
	// ErrNoCheckpoint: the requested checkpoint does not exist.
	ErrNoCheckpoint = errors.New("no such checkpoint")
	// ErrTooLarge: the snapshot would store more new content than allowed.
	ErrTooLarge = errors.New("checkpoint skipped: changed files exceed the size limit")
)

// safetyPrefix starts the label of the checkpoint a restore saves first; the
// restored checkpoint's seq follows it, which is what lets Undo keep walking
// back instead of bouncing between two states.
const safetyPrefix = "before restore [seq "

// IsSafety reports whether cp was saved by a restore rather than by a turn.
func IsSafety(cp Checkpoint) bool { return strings.HasPrefix(cp.Label, safetyPrefix) }

func safetyTarget(cp Checkpoint) (int, bool) {
	if !IsSafety(cp) {
		return 0, false
	}
	rest := cp.Label[len(safetyPrefix):]
	n, err := strconv.Atoi(rest[:max(strings.IndexByte(rest, ']'), 0)])
	return n, err == nil
}

var paneRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Checkpoint is one stored snapshot. N is its position, 1 = newest.
type Checkpoint struct {
	N      int
	Seq    int
	Commit string
	Tree   string
	Label  string
	Time   time.Time
}

// Store is one pane's checkpoints in one work tree.
type Store struct {
	root string
	pane string
	// extra is the -c list from loadFilters.
	extra []string
	// Keep is how many checkpoints survive a Save (0 = DefaultKeep).
	Keep int
	// MaxNewBytes is the size guard (0 = DefaultMaxNewBytes, <0 = off).
	MaxNewBytes int64
}

// Open resolves dir to its work-tree top level. pane must be a plain id.
func Open(dir, pane string) (*Store, error) {
	if !paneRe.MatchString(pane) || strings.Contains(pane, "..") {
		return nil, fmt.Errorf("checkpoint: invalid pane id %q", pane)
	}
	top, err := git(context.Background(), dir, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return nil, fmt.Errorf("%w: %s", ErrNotRepo, dir)
	}
	return &Store{root: top, pane: pane}, nil
}

// Root is the work tree's top level.
func (s *Store) Root() string { return s.root }

// Pane is the pane id the checkpoints belong to.
func (s *Store) Pane() string { return s.pane }

func (s *Store) keep() int {
	if s.Keep > 0 {
		return s.Keep
	}
	return DefaultKeep
}

// Save snapshots the work tree. When the tree equals the newest checkpoint's
// nothing is stored and that checkpoint is returned with created=false.
func (s *Store) Save(ctx context.Context, label string) (Checkpoint, bool, error) {
	s.loadFilters(ctx)
	if err := s.sizeGuard(ctx); err != nil {
		return Checkpoint{}, false, err
	}
	tree, err := s.snapshotTree(ctx)
	if err != nil {
		return Checkpoint{}, false, err
	}
	list, err := s.List(ctx)
	if err != nil {
		return Checkpoint{}, false, err
	}
	if len(list) > 0 && list[0].Tree == tree {
		return list[0], false, nil
	}
	seq := 1
	if len(list) > 0 {
		seq = list[0].Seq + 1
	}
	label = cleanLabel(label)
	commit, err := s.git(ctx, identityEnv(), "commit-tree", tree, "-m", label+"\n\nrysh checkpoint pane="+s.pane)
	if err != nil {
		return Checkpoint{}, false, err
	}
	ref := fmt.Sprintf("%s%s/%08d", refPrefix, s.pane, seq)
	// The empty old value makes this create-only: a concurrent Save that took
	// the same seq fails here instead of overwriting.
	if _, err := s.git(ctx, nil, "update-ref", ref, commit, ""); err != nil {
		return Checkpoint{}, false, err
	}
	s.prune(ctx, append([]Checkpoint{{Seq: seq}}, list...))
	list, err = s.List(ctx)
	if err != nil || len(list) == 0 {
		return Checkpoint{}, false, fmt.Errorf("checkpoint: stored %s but cannot list it: %v", ref, err)
	}
	return list[0], true, nil
}

// List returns the pane's checkpoints, newest first.
func (s *Store) List(ctx context.Context) ([]Checkpoint, error) {
	out, err := s.git(ctx, nil, "for-each-ref",
		"--format=%(refname)%00%(objectname)%00%(tree)%00%(committerdate:unix)%00%(contents:subject)",
		refPrefix+s.pane+"/")
	if err != nil {
		return nil, err
	}
	var list []Checkpoint
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 5 {
			continue
		}
		seq, err := strconv.Atoi(f[0][strings.LastIndex(f[0], "/")+1:])
		if err != nil {
			continue
		}
		ts, _ := strconv.ParseInt(f[3], 10, 64)
		list = append(list, Checkpoint{Seq: seq, Commit: f[1], Tree: f[2], Time: time.Unix(ts, 0), Label: f[4]})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq > list[j].Seq })
	for i := range list {
		list[i].N = i + 1
	}
	return list, nil
}

func (s *Store) get(ctx context.Context, n int) (Checkpoint, error) {
	list, err := s.List(ctx)
	if err != nil {
		return Checkpoint{}, err
	}
	if n < 1 || n > len(list) {
		return Checkpoint{}, fmt.Errorf("%w: #%d (pane has %d)", ErrNoCheckpoint, n, len(list))
	}
	return list[n-1], nil
}

// Diff summarises what changed between checkpoint n and the work tree now.
func (s *Store) Diff(ctx context.Context, n int) (string, error) {
	s.loadFilters(ctx)
	cp, err := s.get(ctx, n)
	if err != nil {
		return "", err
	}
	now, err := s.snapshotTree(ctx)
	if err != nil {
		return "", err
	}
	return s.git(ctx, nil, "diff-tree", "-r", "--no-renames", "--stat", cp.Tree, now)
}

// Restore writes checkpoint n back into the work tree. The state it replaces
// is saved first and returned as safety (it is the new #1, unless the tree was
// already identical to the newest checkpoint).
func (s *Store) Restore(ctx context.Context, n int) (restored, safety Checkpoint, err error) {
	s.loadFilters(ctx)
	target, err := s.get(ctx, n)
	if err != nil {
		return Checkpoint{}, Checkpoint{}, err
	}
	// The safety save is not size-guarded: losing the user's state to a guard
	// would be worse than storing it.
	guard := s.MaxNewBytes
	s.MaxNewBytes = -1
	safety, _, err = s.Save(ctx, fmt.Sprintf("%s%d]: %s", safetyPrefix, target.Seq, target.Label))
	s.MaxNewBytes = guard
	if err != nil {
		return Checkpoint{}, Checkpoint{}, fmt.Errorf("checkpoint: safety save failed, nothing restored: %w", err)
	}
	if safety.Tree == target.Tree {
		return target, safety, nil
	}
	// Paths in the current snapshot that the target lacks are removed; paths
	// the target has that differ (or are missing now) are written.
	added, err := s.diffNames(ctx, target.Tree, safety.Tree, "A")
	if err != nil {
		return Checkpoint{}, Checkpoint{}, err
	}
	toWrite, err := s.diffNames(ctx, target.Tree, safety.Tree, "DMT")
	if err != nil {
		return Checkpoint{}, Checkpoint{}, err
	}
	for _, p := range added {
		err := removeInRepo(s.root, p)
		if errors.Is(err, errOutsideRepo) {
			continue // changed under us into a path that leaves the repo: leave it
		}
		if err != nil {
			return Checkpoint{}, Checkpoint{}, fmt.Errorf("checkpoint: remove %s: %w", p, err)
		}
	}
	if len(toWrite) > 0 {
		idx, cleanup, err := tempIndex()
		if err != nil {
			return Checkpoint{}, Checkpoint{}, err
		}
		defer cleanup()
		env := []string{"GIT_INDEX_FILE=" + idx}
		if _, err := s.git(ctx, env, "read-tree", target.Tree); err != nil {
			return Checkpoint{}, Checkpoint{}, err
		}
		stdin := []byte(strings.Join(toWrite, "\x00") + "\x00")
		if _, err := s.gitIn(ctx, env, stdin, "checkout-index", "-f", "-z", "--stdin"); err != nil {
			return Checkpoint{}, Checkpoint{}, err
		}
	}
	return target, safety, nil
}

// Undo restores the state before the latest turn: the newest turn checkpoint
// that differs from the work tree now. Repeated, it steps further back: when
// the work tree still equals what the last restore wrote, the search starts
// below that checkpoint. Safety checkpoints are never undo targets; they stay
// listed for ##checkpoint restore.
func (s *Store) Undo(ctx context.Context) (restored, safety Checkpoint, err error) {
	s.loadFilters(ctx)
	now, err := s.snapshotTree(ctx)
	if err != nil {
		return Checkpoint{}, Checkpoint{}, err
	}
	list, err := s.List(ctx)
	if err != nil {
		return Checkpoint{}, Checkpoint{}, err
	}
	below := int(^uint(0) >> 1)
	if len(list) > 0 {
		if k, ok := safetyTarget(list[0]); ok {
			for _, cp := range list {
				if cp.Seq == k && cp.Tree == now {
					below = k
				}
			}
		}
	}
	for _, cp := range list {
		if cp.Seq >= below || IsSafety(cp) || cp.Tree == now {
			continue
		}
		return s.Restore(ctx, cp.N)
	}
	return Checkpoint{}, Checkpoint{}, fmt.Errorf("%w: nothing older to undo to", ErrNoCheckpoint)
}

func (s *Store) diffNames(ctx context.Context, from, to, filter string) ([]string, error) {
	out, err := s.git(ctx, nil, "diff-tree", "-r", "-z", "--no-renames", "--name-only", "--diff-filter="+filter, from, to)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(out, "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// snapshotTree writes the work tree as `git add -A` sees it into a temporary
// index and returns the tree id.
//
// The temporary index is seeded from a COPY of the user's index when there is
// one: its stat cache lets `add -A` rehash only the files that changed, where
// an index rebuilt from HEAD would hash every tracked file on every turn. The
// copy is read, never the original written. Any failure on that path (a split
// or otherwise unusual index) falls back to seeding from HEAD.
func (s *Store) snapshotTree(ctx context.Context) (string, error) {
	if tree, err := s.snapshotFrom(ctx, true); err == nil {
		return tree, nil
	}
	return s.snapshotFrom(ctx, false)
}

func (s *Store) snapshotFrom(ctx context.Context, fromUserIndex bool) (string, error) {
	idx, cleanup, err := tempIndex()
	if err != nil {
		return "", err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + idx}
	if fromUserIndex {
		src, err := s.git(ctx, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
		if err != nil {
			return "", err
		}
		if err := copyFile(src, idx); err != nil {
			return "", err
		}
	} else if _, err := s.git(ctx, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err == nil {
		if _, err := s.git(ctx, env, "read-tree", "HEAD"); err != nil {
			return "", err
		}
	}
	if _, err := s.git(ctx, env, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	return s.git(ctx, env, "write-tree")
}

// copyFile copies src to dst and gives dst src's mtime. For an index that is
// not cosmetic: git re-hashes an entry whose mtime is not older than the index
// file's ("racy"), and a copy stamped "now" would make every entry look safely
// older, so a same-size rewrite in the index's tick would keep the old blob.
func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}

// loadFilters neutralises every configured clean/smudge/process filter so a
// repository's config cannot make the daemon run a command. Every git call of
// an operation carries the result: not only add and checkout-index but also
// ls-files -m and even write-tree can run a clean filter (seen in the
// TestRepoConfigCannotRunCode trace). It is re-read at the start of each
// public operation, so a filter configured after Open is covered too.
func (s *Store) loadFilters(ctx context.Context) []string {
	out, _ := git(ctx, s.root, nil, nil, "config", "--name-only", "--get-regexp", `^filter\..*\.(clean|smudge|process|required)$`)
	var extra []string
	for _, name := range strings.Fields(out) {
		if strings.HasSuffix(name, ".required") {
			extra = append(extra, "-c", name+"=false")
		} else {
			extra = append(extra, "-c", name+"=")
		}
	}
	s.extra = extra
	return extra
}

// sizeGuard refuses a snapshot whose modified + untracked files exceed the
// limit, before anything is written to the object store.
func (s *Store) sizeGuard(ctx context.Context) error {
	limit := s.MaxNewBytes
	if limit == 0 {
		limit = DefaultMaxNewBytes
	}
	if limit < 0 {
		return nil
	}
	out, err := s.git(ctx, nil, "ls-files", "-z", "-m", "-o", "--exclude-standard")
	if err != nil {
		return err
	}
	var total int64
	for _, p := range strings.Split(out, "\x00") {
		if p == "" {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(p))); err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
		if total > limit {
			return fmt.Errorf("%w (%d bytes)", ErrTooLarge, limit)
		}
	}
	return nil
}

func (s *Store) prune(ctx context.Context, list []Checkpoint) {
	keep := s.keep()
	for i, cp := range list {
		if i < keep {
			continue
		}
		_, _ = s.git(ctx, nil, "update-ref", "-d", fmt.Sprintf("%s%s/%08d", refPrefix, s.pane, cp.Seq))
	}
}

func cleanLabel(l string) string {
	l = strings.Join(strings.Fields(l), " ")
	if l == "" {
		l = "checkpoint"
	}
	if len(l) > 200 {
		l = l[:200]
	}
	return l
}

func identityEnv() []string {
	return []string{
		"GIT_AUTHOR_NAME=rysh", "GIT_AUTHOR_EMAIL=checkpoint@rysh.invalid",
		"GIT_COMMITTER_NAME=rysh", "GIT_COMMITTER_EMAIL=checkpoint@rysh.invalid",
	}
}

func tempIndex() (string, func(), error) {
	d, err := os.MkdirTemp("", "rysh-checkpoint-")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(d, "index"), func() { _ = os.RemoveAll(d) }, nil
}

var errOutsideRepo = errors.New("checkpoint: path leaves the repository")

// removeInRepo removes the file at rel (slash-separated, relative to root),
// then any directories it leaves empty. The path is checked when it is used,
// not only when it was listed: every directory on the way must still be a real
// directory, because one swapped for a symlink since the snapshot would aim the
// delete outside the repository. A symlink at rel itself is removed, never its
// target.
func removeInRepo(root, rel string) error {
	full := filepath.Join(root, filepath.FromSlash(rel))
	if !within(root, full) {
		return errOutsideRepo
	}
	for dir := filepath.Dir(full); within(root, dir); dir = filepath.Dir(dir) {
		fi, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			return nil // gone already, and so is the file
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() { // a symlink, or a file where a directory was
			return errOutsideRepo
		}
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return err
	}
	removeEmptyParents(root, filepath.Dir(full))
	return nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func removeEmptyParents(root, dir string) {
	for within(root, dir) {
		if os.Remove(dir) != nil { // not empty, or gone
			return
		}
		dir = filepath.Dir(dir)
	}
}

func (s *Store) git(ctx context.Context, env []string, args ...string) (string, error) {
	return gitIn(ctx, s.root, env, s.extra, nil, args...)
}

func (s *Store) gitIn(ctx context.Context, env []string, stdin []byte, args ...string) (string, error) {
	return gitIn(ctx, s.root, env, s.extra, stdin, args...)
}

func git(ctx context.Context, dir string, env, extra []string, args ...string) (string, error) {
	return gitIn(ctx, dir, env, extra, nil, args...)
}

func gitIn(ctx context.Context, dir string, env, extra []string, stdin []byte, args ...string) (string, error) {
	base := []string{"-C", dir, "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "commit.gpgsign=false", "-c", "core.quotepath=false"}
	base = append(base, extra...)
	base = append(base, args...)
	cmd := exec.CommandContext(ctx, "git", base...)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("checkpoint: git %s: %s: %w", args[0], strings.TrimSpace(stderr.String()), err)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}
