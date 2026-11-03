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
	"strconv"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/ansakeys"
	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const ansaUsageText = "usage: rysh ansa send <@name|pane-id> [--as <pane-id>] [--session <name>] -- <text>\n" +
	"       rysh ansa prompt <@name|pane-id> [--as <pane-id>] -- <text>\n" +
	"       rysh ansa keys <@name|pane-id> [--as <pane-id>] [--session <name>] [--force] -- <key>...\n" +
	"       rysh ansa keys --claude-session <id> [--as <pane-id>] [--session <name>] [--force] -- <key>...\n" +
	"       rysh ansa hung <@name|pane-id> | --claude-session <id> | --all [--minutes N] [--session <name>]\n" +
	"       --as defaults to $RYSH_PANE\n" +
	"       keys: esc, esc*2, up, down, left, right, backspace, ctrl+u, … ; enter, tab,\n" +
	"       ctrl+c, ctrl+d and other keys that submit or signal need --force\n" +
	"       every keys send is logged to <session dir>/<session>/ansa-keys-audit.jsonl\n" +
	"       hung: HUNG = claude's working marker on screen + transcript file unchanged N min\n" +
	"       (default 10); reads file times and the screen only, never transcript content"

// runAnsaCmd implements `rysh ansa <send|prompt> <target> [flags] <text>`.
func runAnsaCmd(cfg config.Config, args []string) error {
	// args[0] == "ansa".
	rest := args[1:]
	if len(rest) == 0 {
		return errors.New(progname.Rewrite(ansaUsageText))
	}

	sub := rest[0]
	rest = rest[1:]

	if sub == "keys" {
		return runAnsaKeysCmd(cfg, rest)
	}
	if sub == "hung" {
		return runAnsaHungCmd(cfg, rest)
	}

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

// runAnsaKeysCmd implements `rysh ansa keys`: press named keys in ONE pane.
//
//	rysh ansa keys @release-prep-worker -- esc
//	rysh ansa keys --claude-session <id> -- esc*2
//	rysh ansa keys <pane-id> --force -- ctrl+u enter
//
// Key names are checked here for a quick local error and again by the daemon,
// which owns the guard (guarded keys need --force) and the audit line.
func runAnsaKeysCmd(cfg config.Config, rest []string) error {
	// Everything after "--" is keys; flags are read only before it.
	var keys []string
	if i := indexOf(rest, "--"); i >= 0 {
		keys = append(keys, rest[i+1:]...)
		rest = rest[:i]
	}
	rest, sess := extractStringFlag(rest, "--session")
	rest, as := extractStringFlag(rest, "--as")
	rest, claudeSession := extractStringFlag(rest, "--claude-session")
	rest, force := extractBoolFlag(rest, "--force")
	if hasFlag(rest, "--claude-session") {
		return errors.New(progname.Rewrite("--claude-session needs a session id\n" + ansaUsageText))
	}

	to := ""
	if claudeSession == "" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New(progname.Rewrite("rysh ansa keys needs a target\n" + ansaUsageText))
		}
		to, rest = rest[0], rest[1:]
	}
	for _, a := range rest {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("unknown flag %q to rysh ansa keys (keys go to ONE pane)\n%s", a,
				progname.Rewrite(ansaUsageText))
		}
	}
	// Keys may also come without "--": the words after the target.
	keys = append(rest, keys...)
	if _, err := ansakeys.Parse(keys); err != nil {
		return fmt.Errorf("%v\n%s", err, progname.Rewrite(ansaUsageText))
	}

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
	resp, err := cli.AnsaKeys(store, sess, as, to, claudeSession, keys, force)
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
		return errors.New("ansa keys failed")
	}
	return nil
}

// runAnsaHungCmd implements `rysh ansa hung`: is the claude in a pane hung?
//
//	rysh ansa hung @release-prep-worker
//	rysh ansa hung --all --minutes 15
//	rysh ansa hung --claude-session <id>
//
// Read-only. The daemon decides; see internal/actors/ansa_hung.go.
func runAnsaHungCmd(cfg config.Config, rest []string) error {
	rest, sess := extractStringFlag(rest, "--session")
	rest, claudeSession := extractStringFlag(rest, "--claude-session")
	rest, minutesText := extractStringFlag(rest, "--minutes")
	rest, all := extractBoolFlag(rest, "--all")
	if hasFlag(rest, "--claude-session") {
		return errors.New(progname.Rewrite("--claude-session needs a session id\n" + ansaUsageText))
	}
	if hasFlag(rest, "--minutes") {
		return errors.New(progname.Rewrite("--minutes needs a number\n" + ansaUsageText))
	}
	minutes := 0
	if minutesText != "" {
		n, err := strconv.Atoi(minutesText)
		if err != nil || n < 1 || n > 24*60 {
			return fmt.Errorf("--minutes must be a whole number from 1 to 1440, got %q", minutesText)
		}
		minutes = n
	}
	to := ""
	for _, a := range rest {
		switch {
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q to rysh ansa hung\n%s", a, progname.Rewrite(ansaUsageText))
		case to != "":
			return fmt.Errorf("rysh ansa hung takes one target, got %q and %q (use --all for every claude pane)", to, a)
		default:
			to = a
		}
	}
	targets := 0
	for _, set := range []bool{to != "", claudeSession != "", all} {
		if set {
			targets++
		}
	}
	if targets != 1 {
		return errors.New(progname.Rewrite("rysh ansa hung needs ONE of: a target, --claude-session <id>, or --all\n" + ansaUsageText))
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
	resp, err := cli.AnsaHung(store, sess, to, claudeSession, all, minutes)
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
		return errors.New("ansa hung failed")
	}
	return nil
}
