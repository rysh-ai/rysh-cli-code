// SPDX-License-Identifier: Apache-2.0

package main

// `rysh steer` (boat gap #6 / rr-order-154 C6): add a message to a pane's
// running turn without stopping it. rysh's own agent gets it queued behind
// the turn; a harness (claude, codex) gets it typed in. See internal/cli
// steer.go. The hosted runner calls it with the text on stdin
// (sh -c '... -- "$(cat)"'), never in a host argv.
//
//	rysh steer [--session <name>] --pane-id <id> -- <text>
//
// Exit codes: 0 delivered, 1 refused or unreachable.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const steerUsage = "usage: rysh steer [--session <name>] --pane-id <id> (--stdin | -- <text>)"

type steerOptions struct {
	Session, PaneID, Text string
}

// parseSteerArgs parses `rysh steer`. With --stdin the text is stdin,
// exactly (the hosted runner's form: the text is never an argv).
func parseSteerArgs(args []string, stdin io.Reader) (steerOptions, error) {
	var o steerOptions
	rest := args[1:]
	var text []string
	hasDash := false
	if i := indexOf(rest, "--"); i >= 0 {
		text, hasDash = rest[i+1:], true
		rest = rest[:i]
	}
	var fromStdin bool
	rest, o.Session = extractStringFlag(rest, "--session")
	rest, o.PaneID = extractStringFlag(rest, "--pane-id")
	rest, fromStdin = extractBoolFlag(rest, "--stdin")
	if len(rest) > 0 || o.PaneID == "" || (fromStdin && hasDash) {
		return o, errors.New(progname.Rewrite(steerUsage))
	}
	if fromStdin {
		b, err := io.ReadAll(io.LimitReader(stdin, steerMaxStdin+1))
		if err != nil || len(b) > steerMaxStdin {
			return o, errors.New("steer: the text on stdin is unreadable or longer than 1 MiB")
		}
		o.Text = string(b)
	} else {
		o.Text = strings.Join(text, " ")
	}
	if strings.TrimSpace(o.Text) == "" {
		return o, errors.New(progname.Rewrite(steerUsage))
	}
	return o, nil
}

const steerMaxStdin = 1 << 20

func runSteerCmd(cfg config.Config, args []string) error {
	o, err := parseSteerArgs(args, os.Stdin)
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
	how, err := cli.PaneSteer(store, o.Session, o.PaneID, o.Text)
	if err != nil {
		return err
	}
	fmt.Printf("steered pane %s: %s\n", o.PaneID, how)
	return nil
}
