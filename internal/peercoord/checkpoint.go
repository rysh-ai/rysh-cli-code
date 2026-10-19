// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
)

// checkpointTask is a narrow daemon capability, not arbitrary git/shell access.
// The local enrollment authorizes workspace writes; the caller supplies only a
// commit message. No paths, refs, repositories, hooks or Git options are accepted.
func checkpointTask(ctx context.Context, b *Board, ep *Endpoint, x *Execution, t *pc.Task, message string) (map[string]any, error) {
	if ep == nil || ep.Permission != "workspace" || x == nil || t == nil || x.TaskID != t.ID || t.Current() == nil || x.AttemptID != t.Current().ID || ep.Agent.ID != t.AgentID || x.AgentID != t.AgentID {
		return nil, fmt.Errorf("checkpoint requires the assigned workspace-enabled task execution")
	}
	if !b.Listening || b.State.Paused || !pc.Active(t.State) || t.State == "cancel_requested" || t.State == "needs_reconciliation" || x.Paused || x.Phase != "running" || time.Now().After(x.Deadline) || !time.Now().Before(t.Current().LeaseUntil) {
		return nil, fmt.Errorf("checkpoint requires a live, unpaused task attempt")
	}
	if strings.TrimSpace(message) == "" || len(message) > 4096 || strings.ContainsRune(message, 0) {
		return nil, fmt.Errorf("checkpoint requires a commit message up to 4096 bytes")
	}
	project, ok := b.Projects[t.Project]
	if !ok {
		return nil, fmt.Errorf("task project is no longer mapped locally")
	}
	info, err := os.Lstat(x.Directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("task worktree is unavailable or replaced")
	}
	// Even an altered .git pointer must not redirect this capability to another
	// project or another branch. Resolve both ends from the owner's saved paths.
	common, err := checkpointGit(ctx, project.Path, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	actual, err := checkpointGit(ctx, x.Directory, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || !samePath(common, actual) {
		return nil, fmt.Errorf("task Git metadata does not belong to the enrolled project")
	}
	top, err := checkpointGit(ctx, x.Directory, nil, "rev-parse", "--show-toplevel")
	if err != nil || !samePath(top, x.Directory) {
		return nil, fmt.Errorf("task worktree root changed")
	}
	branch, err := checkpointGit(ctx, x.Directory, nil, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != x.Branch {
		return nil, fmt.Errorf("task branch changed; owner review required")
	}
	// git add can invoke clean/process filters. Neutralize all configured filters
	// in addition to hooks; the daemon must not execute commands from task files.
	filterNames, err := checkpointGit(ctx, x.Directory, nil, "config", "--name-only", "--get-regexp", `^filter\..*\.(clean|process|required)$`)
	if err != nil {
		var exit *exec.ExitError
		if !(errors.As(err, &exit) && exit.ExitCode() == 1) {
			return nil, err
		}
	}
	extra := []string{}
	for _, name := range strings.Fields(filterNames) {
		if strings.HasSuffix(name, ".clean") || strings.HasSuffix(name, ".process") {
			extra = append(extra, "-c", name+"=")
		}
		if strings.HasSuffix(name, ".required") {
			extra = append(extra, "-c", name+"=false")
		}
	}
	if _, err = checkpointGit(ctx, x.Directory, extra, "add", "--all", "--", "."); err != nil {
		return nil, err
	}
	_, diffErr := checkpointGit(ctx, x.Directory, extra, "diff", "--cached", "--quiet", "--no-ext-diff")
	changed := diffErr != nil
	if changed {
		var exit *exec.ExitError
		if !(errors.As(diffErr, &exit) && exit.ExitCode() == 1) {
			return nil, diffErr
		}
		if _, err = checkpointGit(ctx, x.Directory, extra, "commit", "--no-verify", "-m", message); err != nil {
			return nil, err
		}
	}
	commit, err := checkpointGit(ctx, x.Directory, nil, "rev-parse", "HEAD")
	return map[string]any{"task_id": t.ID, "attempt_id": x.AttemptID, "commit": commit, "branch": x.Branch, "changed": changed, "artifact_scope": "local worktree; not transferred"}, err
}

func samePath(a, b string) bool {
	a, e1 := filepath.EvalSymlinks(a)
	b, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && filepath.Clean(a) == filepath.Clean(b)
}

func checkpointGit(ctx context.Context, dir string, extra []string, args ...string) (string, error) {
	base := []string{"-C", dir, "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "commit.gpgsign=false"}
	base = append(base, extra...)
	base = append(base, args...)
	cmd := exec.CommandContext(ctx, "git", base...)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	data, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(data)), fmt.Errorf("task checkpoint git %s: %s: %w", args[0], strings.TrimSpace(string(data)), err)
	}
	return strings.TrimSpace(string(data)), nil
}
