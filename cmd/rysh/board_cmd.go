// SPDX-License-Identifier: Apache-2.0

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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const boardUsage = "usage: rysh board post [--as <pane-id>] [--kind <kind>] [--thread <id>] " +
	"[--board <id>] [--session <name>] -- <text>\n" +
	"       rysh board reply <thread-id> [--as <pane-id>] [--kind <kind>] " +
	"[--board <id>] [--session <name>] -- <text>\n" +
	"       rysh board tail [--since <unix-millis>] [--limit <threads>] [--json] " +
	"[--board <id>] [--session <name>]\n" +
	"       --as defaults to $RYSH_PANE; --board defaults to the poster pane's own board " +
	"(the session board unless it has board.id meta); flags may appear in any order"

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

	// board names which board to post to or read (design 028). Empty means
	// "let the daemon resolve it from the poster's pane", which is what keeps
	// every brief written before board ids existed working unchanged.
	board string

	// tail only.
	since   int64
	limit   int
	jsonOut bool
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
	if b.sub != "post" && b.sub != "reply" && b.sub != "tail" {
		return b, fmt.Errorf("unknown board subcommand %q\n%s", b.sub, progname.Rewrite(boardUsage))
	}

	rest, b.sess = extractStringFlag(rest, "--session")

	// --board is extracted for EVERY subcommand, ABOVE the tail early-return.
	//
	// FOUND LIVE, 2026-08-11: it used to be extracted only on the post/reply
	// path, so `board tail --board epic-07` reached parseBoardTailArgs with the
	// flag still in the argument list and died as "takes no positional
	// arguments" — a read path that could name a board in the usage text and
	// refuse it in practice. This is F-19's rule exactly (a flag that works in
	// one position only), which is why it now sits with --session, the other
	// flag every subcommand shares.
	rest, boardFlag := extractStringFlag(rest, "--board")
	if boardFlag == "" {
		// `--fleet <name>` is the same flag under the name a fleet operator
		// reaches for. Until the fleet actor owns the mapping (E-40), a
		// fleet's board IS its name.
		rest, boardFlag = extractStringFlag(rest, "--fleet")
	}
	if boardFlag != "" {
		if err := msg.ValidateBoardID(boardFlag); err != nil {
			return b, fmt.Errorf("%v\n%s", err, progname.Rewrite(boardUsage))
		}
		b.board = msg.NormalizeBoardID(boardFlag)
	}

	// tail is the READ path and shares nothing with post/reply below except
	// --session: no poster, no kind, no thread, no text.
	if b.sub == "tail" {
		return parseBoardTailArgs(b, rest)
	}

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

// parseBoardTailArgs parses the flags of `board tail`.
//
// Every flag is extracted by name from anywhere in the argument list, which is
// the F-19 rule: a flag that works in one position only, and is undocumented,
// is how that defect stayed invisible for a whole epic. tail has no positional
// argument at all, and an unrecognised leftover is REFUSED rather than ignored
// — a silently dropped `--limt 5` would hand back the whole board while the
// caller believed it had asked for five threads.
func parseBoardTailArgs(b boardArgs, rest []string) (boardArgs, error) {
	rest, sinceFlag := extractStringFlag(rest, "--since")
	rest, limitFlag := extractStringFlag(rest, "--limit")
	rest, b.jsonOut = extractBoolFlag(rest, "--json")

	if sinceFlag != "" {
		v, err := strconv.ParseInt(sinceFlag, 10, 64)
		if err != nil || v < 0 {
			return b, fmt.Errorf(
				"rysh board tail: --since wants unix millis, got %q\n%s",
				sinceFlag, progname.Rewrite(boardUsage))
		}
		b.since = v
	}
	if limitFlag != "" {
		v, err := strconv.Atoi(limitFlag)
		if err != nil || v < 0 {
			return b, fmt.Errorf(
				"rysh board tail: --limit wants a non-negative thread count, got %q\n%s",
				limitFlag, progname.Rewrite(boardUsage))
		}
		b.limit = v
	}

	// A bare `--` is harmless; anything after it is not, because tail takes no
	// text and quietly discarding it would look like it had been understood.
	if i := indexOf(rest, "--"); i >= 0 {
		rest = append(rest[:i:i], rest[i+1:]...)
	}
	if len(rest) > 0 {
		return b, fmt.Errorf(
			"rysh board tail takes no positional arguments, got %q\n%s",
			strings.Join(rest, " "), progname.Rewrite(boardUsage))
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
	if p.sub == "tail" {
		return runBoardTail(cfg, p)
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

	resp, err := cli.BoardPost(store, sess, as, kind, threadID, p.board, text)
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

// runBoardTail implements `rysh board tail` — the board's read path, and the
// first way an agent inside a pane can see what the fleet has been saying
// without a TUI.
//
// THE ONE BEHAVIOUR THIS FUNCTION EXISTS TO GET RIGHT: an unanswered query is
// reported as an ERROR, never as an empty board. `{"threads":[]}` on stdout
// with exit 0 is a claim that the fleet said nothing, and when the recorder is
// dead that claim is false in the most confident-looking way there is. It is
// the F-20 shape (a roster that read "1 agent" because nothing was listening)
// and the F-23 shape (a restore against the wrong bucket that failed looking
// healthy), and both cost this track days.
func runBoardTail(cfg config.Config, p boardArgs) error {
	sess := p.sess
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

	reply, err := cli.BoardTail(store, sess, p.board, board.Query{Since: p.since, Limit: p.limit})
	if err != nil {
		if errors.Is(err, board.ErrNoRecorder) {
			return boardTailNoRecorderError(err)
		}
		return err
	}

	if p.jsonOut {
		enc, merr := json.MarshalIndent(reply, "", "  ")
		if merr != nil {
			return merr
		}
		fmt.Println(string(enc))
		return nil
	}
	fmt.Print(renderBoardTail(reply))
	return nil
}

// boardTailNoRecorderError is what a reader is told when nobody answered.
//
// Extracted so the WORDING is regression-testable without a daemon, because
// the wording is the whole safety argument on this surface. Nothing goes to
// stdout on this path — not even an empty JSON document — so a script doing
// `rysh board tail --json | jq` gets a non-zero status and nothing to parse,
// rather than a convincing empty answer.
func boardTailNoRecorderError(err error) error {
	return fmt.Errorf("%s\n  underlying: %w", progname.Rewrite(
		"rysh board tail: the board recorder did not answer.\n"+
			"  THIS IS NOT AN EMPTY BOARD — nothing can be said about what was posted.\n"+
			"  The session's listener (ABLA) is not running, or this session has no bus."),
		err)
}

// renderBoardTail formats a recorder's answer for a human (or for a claude
// reading its own terminal). Pure, so the empty-board wording below is
// regression-testable without a daemon.
func renderBoardTail(r *board.QueryReply) string {
	var b strings.Builder
	fmt.Fprintf(&b, "AGENTS-BOARD | %d threads · %d posts · %d agents\n",
		r.Stats.Threads, r.Stats.Posts, len(r.Roster))

	if len(r.Threads) == 0 {
		// "the recorder answered" is load-bearing wording, not decoration. It
		// is the only thing distinguishing this output from the failure
		// runBoardTail refuses to print, and a reader has to be able to tell
		// them apart at a glance.
		if r.Filtered > 0 {
			fmt.Fprintf(&b, "\nthe recorder answered: no threads in this window (%d filtered out by --since)\n", r.Filtered)
		} else {
			b.WriteString("\nthe recorder answered: the board is empty\n")
		}
		return b.String() + boardTailNotices(r)
	}

	for _, t := range r.Threads {
		b.WriteString("\n")
		if t.Root != nil {
			fmt.Fprintf(&b, "● %s  [%s]  %s%s\n",
				t.Root.Persona, t.Root.Kind, boardTailClock(t.Root.TS), boardTailKey(t.Key))
			fmt.Fprintf(&b, "  %s\n", t.Root.Text)
		} else {
			// A provisional thread: replies arrived before their root, which is
			// expected rather than an error (design 025 §4.3). Say so instead
			// of rendering a reply as if it were a root.
			fmt.Fprintf(&b, "◌ (root has not arrived) %s\n", boardTailKey(t.Key))
		}
		for _, rep := range t.Replies {
			fmt.Fprintf(&b, "  ↳ %s  [%s]  %s\n", rep.Persona, rep.Kind, boardTailClock(rep.TS))
			fmt.Fprintf(&b, "    %s\n", rep.Text)
		}
	}
	return b.String() + boardTailNotices(r)
}

// boardTailNotices says what the answer is NOT showing.
//
// Design 025 §7.1a's discipline applied to the read path: a bounded view that
// does not disclose its bound presents a window as the whole board.
func boardTailNotices(r *board.QueryReply) string {
	var notes []string
	if r.Withheld > 0 {
		notes = append(notes, fmt.Sprintf("%d older threads not shown (--limit)", r.Withheld))
	}
	if r.Filtered > 0 && len(r.Threads) > 0 {
		notes = append(notes, fmt.Sprintf("%d threads outside the --since window", r.Filtered))
	}
	if r.Stats.Evicted > 0 {
		notes = append(notes, fmt.Sprintf("%d posts evicted from the recorder's memory", r.Stats.Evicted))
	}
	if r.Stats.Provisional > 0 {
		notes = append(notes, fmt.Sprintf("%d threads still missing their root", r.Stats.Provisional))
	}
	// F-26. The recorder could not confirm who is alive, so the agent count may
	// include panes that have closed. Said out loud rather than left for the
	// reader to discover, on the same principle as every other counter here: a
	// roster that overcounts looks authoritative and is wrong.
	if !r.RosterReconciled && len(r.Roster) > 0 {
		notes = append(notes, "agent list not confirmed against live panes — it may name closed ones")
	}
	if len(notes) == 0 {
		return ""
	}
	return "\n(" + strings.Join(notes, "; ") + ")\n"
}

// boardTailKey renders a thread key as something a reader can USE.
//
// A thread key is an address: it is what you pass to `rysh board reply`. A
// standalone thread has no usable address — the store minted it a key behind a
// NUL byte precisely so no agent could name it (design 025 §4.3a) — so
// printing that key would offer the reader an address that cannot work, and
// print a raw control character while doing it. The live run caught this: the
// NUL rendered as an invisible byte before "standalone/3", which looks like a
// stray space and is worse than either alternative.
func boardTailKey(key string) string {
	if key == "" || board.IsStandaloneKey(key) {
		return "  (standalone — no replies possible)"
	}
	return "  " + key
}

// boardTailClock renders a poster's clock. TS is the POSTER's clock and the
// board is arrival-ordered, not causally ordered (design 025 §7.8), so this is
// a label rather than a sequence.
func boardTailClock(ts int64) string {
	if ts <= 0 {
		return "--:--:--"
	}
	return time.UnixMilli(ts).Format("15:04:05")
}
