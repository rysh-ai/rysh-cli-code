// SPDX-License-Identifier: Apache-2.0

package main

// `rysh undo` — put back a pane's files as they were before the agent's last
// turn (gap G7, out/gap09-design.md).
//
//	rysh undo [--session <n>] [--pane-id <p>] [--to <n> | --list]
//
// A thin wrapper over `##checkpoint undo|restore|list`: the snapshots, and
// the rules about what a restore may touch, live in one place
// (internal/checkpoint). --pane-id defaults to $RYSH_PANE, so inside a pane
// plain `rysh undo` acts on that pane. Run again, it steps further back; the
// files each undo replaced are kept as checkpoint #1.

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const undoUsage = "usage: rysh undo [--session <name>] [--pane-id <pane>] [--to <n> | --list]"

type undoOptions struct {
	Session string
	PaneID  string
	Command string
}

func parseUndoArgs(args []string, envPane string) (undoOptions, error) {
	var o undoOptions
	rest := args[1:]
	var to string
	var list bool
	rest, o.Session = extractStringFlag(rest, "--session")
	rest, o.PaneID = extractStringFlag(rest, "--pane-id")
	rest, to = extractStringFlag(rest, "--to")
	rest, list = extractBoolFlag(rest, "--list")
	if len(rest) > 0 {
		return o, fmt.Errorf("unexpected argument %q\n%s", rest[0], progname.Rewrite(undoUsage))
	}
	if o.PaneID == "" {
		o.PaneID = strings.TrimSpace(envPane)
	}
	if o.PaneID == "" {
		return o, errors.New(progname.Rewrite("no pane: pass --pane-id <pane>, or run inside a pane where $RYSH_PANE is set\n" + undoUsage))
	}
	switch {
	case to != "" && list:
		return o, errors.New(progname.Rewrite("pass either --to or --list\n" + undoUsage))
	case to != "":
		n, err := strconv.Atoi(to)
		if err != nil || n < 1 {
			return o, fmt.Errorf("invalid --to %q (want a checkpoint number, 1 = newest)", to)
		}
		o.Command = "checkpoint restore " + strconv.Itoa(n)
	case list:
		o.Command = "checkpoint list"
	default:
		o.Command = "checkpoint undo"
	}
	return o, nil
}

// runUndoCmd implements `rysh undo`.
func runUndoCmd(cfg config.Config, args []string) error {
	o, err := parseUndoArgs(args, os.Getenv("RYSH_PANE"))
	if err != nil {
		return err
	}
	if o.Session == "" {
		o.Session = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}
	resp, err := cli.RyshCommandResult(store, o.Session, "", o.PaneID, o.Command)
	if err != nil {
		return err
	}
	if out := strings.TrimRight(resp.Output, "\n"); out != "" {
		fmt.Println(out)
	}
	if !resp.OK {
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		return errors.New("undo failed")
	}
	return nil
}
