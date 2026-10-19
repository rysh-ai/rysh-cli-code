// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"context"
	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func checkpointFixture(t *testing.T) (*Board, *Endpoint, *Execution, *pc.Task) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	wt := filepath.Join(root, "worktree")
	os.Mkdir(repo, 0700)
	for _, a := range [][]string{{"init"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.invalid"}, {"commit", "--allow-empty", "-m", "base"}, {"worktree", "add", "-b", "peer/task", wt}} {
		if _, err := git(ctx, repo, a...); err != nil {
			t.Fatal(err)
		}
	}
	task := &pc.Task{ID: "task", Project: "math", AgentID: "agent", State: "running", Attempts: []pc.Attempt{{ID: "attempt", State: "running", LeaseUntil: time.Now().Add(time.Minute)}}}
	b := &Board{Listening: true, Projects: map[string]Project{"math": {Path: repo}}}
	ep := &Endpoint{Permission: "workspace", Agent: pc.Agent{ID: "agent"}}
	x := &Execution{ID: "execution", TaskID: "task", AttemptID: "attempt", AgentID: "agent", Directory: wt, Branch: "peer/task", Phase: "running", Deadline: time.Now().Add(time.Minute)}
	return b, ep, x, task
}
func TestTaskCheckpointCommitsOnlyOwnedWorktreeAndDeduplicates(t *testing.T) {
	b, ep, x, task := checkpointFixture(t)
	ctx := context.Background()
	repo := b.Projects["math"].Path
	os.WriteFile(filepath.Join(x.Directory, "answer.txt"), []byte("524\n"), 0600)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	os.WriteFile(filepath.Join(repo, ".git/hooks/post-commit"), []byte("#!/bin/sh\ntouch "+ShellQuote(marker)+"\n"), 0700)
	// A malicious filter would run during add unless the capability disables it.
	git(ctx, repo, "config", "filter.test.clean", "touch "+ShellQuote(marker)+"; cat")
	os.WriteFile(filepath.Join(x.Directory, ".gitattributes"), []byte("answer.txt filter=test\n"), 0600)
	result, err := checkpointTask(ctx, b, ep, x, task, "Save verified count")
	if err != nil {
		t.Fatal(err)
	}
	if result["changed"] != true {
		t.Fatal(result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("hook or filter executed")
	}
	again, err := checkpointTask(ctx, b, ep, x, task, "Save verified count")
	if err != nil || again["changed"] != false || again["commit"] != result["commit"] {
		t.Fatalf("repeat checkpoint: %+v %v", again, err)
	}
	mainHead, _ := git(ctx, repo, "rev-parse", "HEAD")
	if mainHead == result["commit"] {
		t.Fatal("modified main branch")
	}
}
func TestTaskCheckpointRefusesReviewForeignAndCancelledAttempts(t *testing.T) {
	b, ep, x, task := checkpointFixture(t)
	ctx := context.Background()
	ep.Permission = "review"
	if _, e := checkpointTask(ctx, b, ep, x, task, "no"); e == nil {
		t.Fatal("review allowed commit")
	}
	ep.Permission = "workspace"
	x.AttemptID = "other"
	if _, e := checkpointTask(ctx, b, ep, x, task, "no"); e == nil {
		t.Fatal("foreign attempt allowed")
	}
	x.AttemptID = "attempt"
	task.State = "cancel_requested"
	if _, e := checkpointTask(ctx, b, ep, x, task, "no"); e == nil {
		t.Fatal("cancelled attempt allowed")
	}
	task.State = "running"
	x.Branch = "main"
	if _, e := checkpointTask(ctx, b, ep, x, task, "no"); e == nil {
		t.Fatal("changed branch allowed")
	}
}
