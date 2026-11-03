// SPDX-License-Identifier: Apache-2.0

package actors

// workspace_checkpoint.go — ##checkpoint and `rysh undo` (gap G7,
// out/gap09-design.md). The snapshots themselves live in internal/checkpoint;
// this file only resolves which work tree a pane edits and prints results.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/checkpoint"
)

const checkpointUsage = "usage: ##checkpoint [list] | save [label] | diff [n] | restore <n> | undo\n"

// checkpointTimeout bounds one command; a snapshot of a huge tree is the slow
// case, and the actor goroutine must not wait on it forever.
const checkpointTimeout = 2 * time.Minute

// handleCheckpointCommand runs ##checkpoint for a pane, in the directory the
// pane's agent works in.
func (w *WorkspaceActor) handleCheckpointCommand(out *strings.Builder, paneID string, args []string) error {
	if strings.TrimSpace(paneID) == "" {
		fmt.Fprint(out, "checkpoint: no active pane\n")
		return errors.New("checkpoint: no active pane")
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkpointTimeout)
	defer cancel()
	return runCheckpointCommand(ctx, out, w.checkpointDir(paneID), paneID, args)
}

// checkpointDir is the directory a pane's checkpoints are taken in: where its
// agent was launched (the shell may have moved since), else the shell's cwd.
func (w *WorkspaceActor) checkpointDir(paneID string) string {
	snap := w.paneSnapshotByID(paneID)
	if snap != nil {
		if d := strings.TrimSpace(snap.Meta[metaAgentCwd]); d != "" {
			return d
		}
	}
	dir, _ := nativeAgentCwd(snap)
	return dir
}

// runCheckpointCommand is the handler body, free of the actor so it is tested
// against real repositories.
func runCheckpointCommand(ctx context.Context, out *strings.Builder, dir, paneID string, args []string) error {
	fail := func(err error) error {
		fmt.Fprintf(out, "checkpoint: %v\n", err)
		return err
	}
	store, err := checkpoint.Open(dir, paneID)
	if err != nil {
		return fail(err)
	}
	sub := "list"
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	num := func(def int) (int, error) {
		if len(args) < 2 {
			if def > 0 {
				return def, nil
			}
			return 0, errors.New(strings.TrimSuffix(checkpointUsage, "\n"))
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			return 0, fmt.Errorf("%s(not a checkpoint number: %q)", checkpointUsage, args[1])
		}
		return n, nil
	}
	switch sub {
	case "list", "ls":
		list, err := store.List(ctx)
		if err != nil {
			return fail(err)
		}
		if len(list) == 0 {
			fmt.Fprintf(out, "no checkpoints for this pane in %s\n", store.Root())
			return nil
		}
		fmt.Fprintf(out, "checkpoints in %s (1 = newest):\n", store.Root())
		for _, cp := range list {
			fmt.Fprintf(out, "  #%-3d %s  %s  %s\n", cp.N, cp.Time.Format("2006-01-02 15:04:05"), shortSHA(cp.Commit), cp.Label)
		}
	case "save":
		cp, created, err := store.Save(ctx, strings.Join(args[1:], " "))
		if err != nil {
			return fail(err)
		}
		if created {
			fmt.Fprintf(out, "saved checkpoint #1 %s %q\n", shortSHA(cp.Commit), cp.Label)
		} else {
			fmt.Fprintf(out, "no change since checkpoint #1 %q\n", cp.Label)
		}
	case "diff":
		n, err := num(1)
		if err != nil {
			return fail(err)
		}
		d, err := store.Diff(ctx, n)
		if err != nil {
			return fail(err)
		}
		if strings.TrimSpace(d) == "" {
			fmt.Fprintf(out, "no change since checkpoint #%d\n", n)
		} else {
			fmt.Fprintf(out, "changes since checkpoint #%d:\n%s\n", n, d)
		}
	case "restore":
		n, err := num(0)
		if err != nil {
			return fail(err)
		}
		restored, _, err := store.Restore(ctx, n)
		if err != nil {
			return fail(err)
		}
		printRestored(out, restored)
	case "undo":
		restored, _, err := store.Undo(ctx)
		if err != nil {
			return fail(err)
		}
		printRestored(out, restored)
	default:
		fmt.Fprint(out, checkpointUsage)
		return fmt.Errorf("unknown ##checkpoint subcommand: %q", sub)
	}
	return nil
}

func printRestored(out *strings.Builder, cp checkpoint.Checkpoint) {
	fmt.Fprintf(out, "restored checkpoint %s %q; the replaced files are saved as #1 (##checkpoint restore 1 brings them back)\n",
		shortSHA(cp.Commit), cp.Label)
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
