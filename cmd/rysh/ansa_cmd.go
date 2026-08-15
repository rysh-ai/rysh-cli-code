// SPDX-License-Identifier: Apache-2.0

package main

// `rysh ansa send` — the agent's door onto the session router.
//
// A real subcommand rather than one of the generated `rysh --<name>` flag
// forms, for the same reason `rysh board post` is: the flag form routes through
// MsgCLIRyshCommand, which focuses the target pane and echoes the command line
// into its output buffers. That is the human-typed behaviour. A control channel
// carrying work orders between dozens of agents must do neither — routing must
// not move the human's cursor as a side effect of agents talking.
//
// Ergonomics, because a control channel nobody reaches for is not a control
// channel: --as defaults to $RYSH_PANE, which every pane shell exports, so the
// line an agent has to remember is
//
//	rysh ansa send @mgr-01 "the tests are green"
//
// The target may be a pane id or an @given-name. The name is resolved at the
// daemon edge; an ambiguous one comes back as a refusal listing the candidate
// ids, never as a guess.
//
// Exit codes: 0 delivered, 1 refused or unreachable.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const ansaUsageText = "usage: rysh ansa send <@name|pane-id> [--as <pane-id>] [--session <name>] -- <text>\n" +
	"       rysh ansa prompt <@name|pane-id> [--as <pane-id>] -- <text>\n" +
	"       --as defaults to $RYSH_PANE"

// runAnsaCmd implements `rysh ansa <send|prompt> <target> [flags] <text>`.
func runAnsaCmd(cfg config.Config, args []string) error {
	// args[0] == "ansa".
	rest := args[1:]
	if len(rest) == 0 {
		return errors.New(progname.Rewrite(ansaUsageText))
	}

	sub := rest[0]
	rest = rest[1:]

	var mode string
	switch sub {
	case "send":
		mode = msg.AnsaModeShell
	case "prompt":
		mode = msg.AnsaModePrompt
	default:
		return fmt.Errorf("unknown ansa subcommand %q\n%s", sub, progname.Rewrite(ansaUsageText))
	}

	// The target is positional and comes first, so the shape matches ##ansa.
	if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
		return errors.New(progname.Rewrite("rysh ansa needs a target\n" + ansaUsageText))
	}
	to := rest[0]
	rest = rest[1:]

	rest, sess := extractStringFlag(rest, "--session")
	rest, as := extractStringFlag(rest, "--as")

	// Everything after "--" is the text verbatim, so a message may begin with a
	// dash without being mistaken for a flag.
	if i := indexOf(rest, "--"); i >= 0 {
		rest = rest[i+1:]
	}
	text := strings.TrimSpace(strings.Join(rest, " "))
	if text == "" {
		return errors.New(progname.Rewrite(ansaUsageText))
	}

	// The SENDER. Unlike the board's poster this is optional — routing from
	// outside a pane is legitimate — but it is never inferred from the active
	// pane, because a wrong "from" is a lie about who is talking.
	if as == "" {
		as = os.Getenv("RYSH_PANE")
	}
	if sess == "" {
		sess = os.Getenv("RYSH_SESSION")
	}
	if sess == "" {
		sess = cfg.SessionName
	}

	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}

	resp, err := cli.AnsaSend(store, sess, as, to, mode, text)
	if err != nil {
		// Transport failure: the route never ran. Distinct from a route the
		// daemon refused, though both exit 1.
		return err
	}
	if out := strings.TrimRight(resp.Output, "\n"); out != "" {
		fmt.Println(out)
	}
	if !resp.OK {
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		return errors.New("ansa route failed")
	}
	return nil
}
