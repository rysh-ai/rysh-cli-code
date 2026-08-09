package main

// `rysh board post` — the agent's door onto the agents board (design 025 §4).
//
// This is a real subcommand and not one of the generated `rysh --<name>` flag
// forms, even though `##board` is in the dispatch table and therefore gets a
// `rysh --board` alias for free. That alias goes through MsgCLIRyshCommand,
// which focuses the target pane and echoes the command line into its output
// buffers — the human-typed behaviour. An agent posting a milestone must do
// neither, so this subcommand sends MsgCLIBoardPost instead, which reaches the
// workspace's board handler directly.
//
// Ergonomics are the point (epic §4.5: "if posting is harder than not posting,
// agents will not post"). --as defaults to $RYSH_PANE, which every pane shell
// already exports (internal/actors/pane_shell.go), so the whole line an agent
// has to remember is:
//
//	rysh board post "finished the codec wiring"
//
// $RYSH_PANE is the posting PROCESS's own pane. That is an explicit identity
// the caller carries, not the workspace's ambient active pane — the thing
// MsgCLIBoardPost refuses to fall back to. Outside a pane the variable is
// unset, and then --as is required rather than guessed.
//
// Exit codes: 0 posted, 1 refused or unreachable.

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

const boardUsage = "usage: rysh board post [--as <pane-id>] [--kind <kind>] [--thread <id>] " +
	"[--session <name>] -- <text>\n" +
	"       rysh board reply <thread-id> [--as <pane-id>] [--kind <kind>] " +
	"[--session <name>] -- <text>\n" +
	"       --as defaults to $RYSH_PANE; flags may appear in any order"

// boardArgs is the parsed form of a `rysh board` invocation. Parsing is split
// out from runBoardCmd so both live-demo defects below are regression-testable
// without standing up a daemon.
type boardArgs struct {
	sub      string
	as       string
	kind     string
	threadID string
	sess     string
	text     string
}

// parseBoardArgs parses `board <post|reply> [flags] [--] <text>`.
//
// FLAGS ARE EXTRACTED BEFORE THE POSITIONAL THREAD ID, and that ordering is the
// fix for a real defect rather than a stylistic choice: the thread id used to be
// read straight off the front, so `board reply --session s <thread> …` died with
// "needs a thread id" because it saw the flag first, while `board post` accepted
// its flags anywhere. Same tool, two argument grammars, and the printed usage
// gave no hint which one you were in.
func parseBoardArgs(args []string) (boardArgs, error) {
	var b boardArgs
	if len(args) == 0 {
		return b, errors.New(progname.Rewrite(boardUsage))
	}
	b.sub = args[0]
	rest := args[1:]
	if b.sub != "post" && b.sub != "reply" {
		return b, fmt.Errorf("unknown board subcommand %q\n%s", b.sub, progname.Rewrite(boardUsage))
	}

	rest, b.sess = extractStringFlag(rest, "--session")
	rest, b.as = extractStringFlag(rest, "--as")
	rest, kindFlag := extractStringFlag(rest, "--kind")
	rest, threadFlag := extractStringFlag(rest, "--thread")

	// Split the verbatim text off first, so a thread id can never be taken
	// from the message body.
	body := rest
	if i := indexOf(rest, "--"); i >= 0 {
		body, rest = rest[i+1:], rest[:i]
	} else {
		rest = nil
	}

	if b.sub == "reply" {
		if len(rest) == 0 {
			return b, errors.New(progname.Rewrite("rysh board reply needs a thread id\n" + boardUsage))
		}
		b.threadID = rest[0]
	}

	if threadFlag != "" {
		b.threadID = threadFlag
	}
	// Kind is keyed off the SUBCOMMAND, not off "does it carry a thread id".
	//
	// The tempting version -- kind = reply whenever threadID != "" -- is wrong,
	// and a live board caught it: `board post --thread <id>` is how an agent
	// OPENS a thread, so the thread's own root carries a thread id and would
	// have been labelled a reply to itself. The actor door
	// (workspace_board.go) derives kind from threadID and is still correct
	// there only because `##board post` cannot take a thread at all -- its
	// threadID is non-empty exactly when the subcommand was `reply`. Same rule,
	// expressed in the terms each door actually has. --kind overrides.
	if b.sub == "reply" {
		b.kind = msg.BoardKindReply
	}
	if kindFlag != "" {
		b.kind = kindFlag
	}
	b.text = strings.TrimSpace(strings.Join(body, " "))
	if b.text == "" {
		return b, errors.New(progname.Rewrite(boardUsage))
	}
	return b, nil
}

// runBoardCmd implements `rysh board <post|reply> [flags] <text>`.
func runBoardCmd(cfg config.Config, args []string) error {
	// args[0] == "board".
	p, err := parseBoardArgs(args[1:])
	if err != nil {
		return err
	}
	as, kind, threadID, sess, text := p.as, p.kind, p.threadID, p.sess, p.text

	if as == "" {
		as = os.Getenv("RYSH_PANE")
	}
	if as == "" {
		// Not a fallback to the active pane, on purpose: an unattributed post
		// is worse than a refused one, because it credits a bystander.
		return errors.New(progname.Rewrite(
			"rysh board: no poster — pass --as <pane-id>, or run from inside a pane where $RYSH_PANE is set"))
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

	resp, err := cli.BoardPost(store, sess, as, kind, threadID, text)
	if err != nil {
		// Transport failure: the post never happened. Distinct from a post the
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
		return errors.New("board post failed")
	}
	return nil
}
