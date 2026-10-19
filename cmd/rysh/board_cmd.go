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

	"github.com/atotto/clipboard"

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
	"       rysh board tail [--view status|prompt|all|flow|team] [--since <unix-millis>] [--limit <threads>] [--agent <persona>] [--last <N>] [--json] " +
	"[--board <id>] [--session <name>]\n" +
	"       rysh board send --to <pane-id|name> [--id <cmd-id>] [--file <path>] [--thread <id>] [--as <pane-id>] " +
	"[--board <id>] [--session <name>] -- <text>\n" +
	"       rysh board ack <cmd-id> [--note <text>] [--as <pane-id>] [--board <id>] [--session <name>]\n" +
	"       rysh board inbox [--all] [--json] [--as <pane-id>] [--board <id>] [--session <name>]\n" +
	"       rysh board listen [--as <pane-id>] [--board <id>] [--session <name>] [--state-dir <dir>] [--once]\n" +
	"       rysh board copy [worker] [N] [--print] [--board <id>] [--session <name>]\n" +
	"       rysh board output [worker] [N] html|pdf [--out <dir>] [--no-open] [--board <id>] [--session <name>]\n" +
	"       --as defaults to $RYSH_PANE; --board defaults to the poster pane's own board " +
	"(the session board unless it has board.id meta); flags may appear in any order\n" +
	"       send gives another agent WORK: the board delivers it into that agent's session, never its keyboard;\n" +
	"       post is status only; inbox lists commands addressed to you (open ones; --all includes answered)\n" +
	"       copy/output/--agent/--last count MESSAGES (a root or a reply), oldest first; a worker is a persona,\n" +
	"       matched exactly, or a pane id; copy writes the local clipboard (--print also prints the text)\n" +
	"       the session is $RYSH_SESSION, found from any directory under the workspace, worktrees included"

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
	agent   string     // item-based: that worker's messages
	last    int        // item-based: the last N messages; 0 = all
	view    board.View // status | prompt | all (design 051); "" = all

	// copy / output: the shared [worker] [N] [html|pdf] grammar.
	items board.ItemArgs
	print bool // copy: also print the copied text
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
	if b.sub != "post" && b.sub != "reply" && b.sub != "tail" && b.sub != "copy" && b.sub != "output" &&
		!isBoardSendSub(b.sub) && b.sub != "listen" {
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
	// copy / output share one grammar with `##board copy|output`
	// (board.ParseItemArgs): positionals in any order, unknowns refused.
	if b.sub == "copy" || b.sub == "output" {
		if b.sub == "copy" {
			rest, b.print = extractBoolFlag(rest, "--print")
		}
		a, err := board.ParseItemArgs(rest, b.sub == "output")
		if err != nil {
			if errors.Is(err, board.ErrNeedsFormat) {
				return b, fmt.Errorf("[board] %s", strings.Replace(err.Error(), "##board", progname.Rewrite("rysh board"), 1))
			}
			return b, fmt.Errorf("rysh board %s: %v\n%s", b.sub, err, progname.Rewrite(boardUsage))
		}
		a.Board = b.board
		b.items = a
		return b, nil
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
	// --to used to be swallowed here without a word. A directed status post
	// that nothing delivers reads like an order that was sent, so it is
	// refused and pointed at the verb that does deliver (design 050).
	for _, a := range rest {
		if a == "--to" || strings.HasPrefix(a, "--to=") {
			return b, errors.New(progname.Rewrite(
				"rysh board " + b.sub + " is a status post and takes no --to; " +
					"use `rysh board send --to <agent> -- <text>` to give another agent work"))
		}
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
	rest, b.agent = extractStringFlag(rest, "--agent")
	rest, viewFlag := extractStringFlag(rest, "--view")
	if viewFlag != "" {
		v, err := board.ParseView(viewFlag)
		if err != nil {
			return b, fmt.Errorf("rysh board tail: %v\n%s", err, progname.Rewrite(boardUsage))
		}
		b.view = v
	}
	rest, lastFlag := extractStringFlag(rest, "--last")
	if lastFlag != "" {
		v, err := strconv.Atoi(lastFlag)
		if err != nil || v < 0 {
			return b, fmt.Errorf(
				"rysh board tail: --last wants a message count, got %q\n%s",
				lastFlag, progname.Rewrite(boardUsage))
		}
		b.last = v
	}

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
	if len(args) > 1 && args[1] == "listen" {
		return runBoardListen(cfg, args[2:])
	}
	if len(args) > 1 && args[1] == "caps" {
		return runBoardCaps(cfg, args[2:])
	}
	if len(args) > 1 && isBoardSendSub(args[1]) {
		sp, err := parseBoardSendArgs(args[1], args[2:])
		if err != nil {
			return err
		}
		return runBoardSendCmd(cfg, sp)
	}
	p, err := parseBoardArgs(args[1:])
	if err != nil {
		return err
	}
	switch p.sub {
	case "tail":
		return runBoardTail(cfg, p)
	case "copy", "output":
		return runBoardItems(cfg, p)
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

	store, sess, err := resolveBoardSession(cfg, sess)
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
	store, sess, err := resolveBoardSession(cfg, p.sess)
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
	if p.view == board.ViewFlow {
		return runBoardTailFlow(store, sess, reply, p)
	}
	if p.view == board.ViewTeam {
		return runBoardTailTeam(store, sess, reply, p)
	}
	if p.view != "" {
		before := len(reply.Threads)
		reply = board.FilterReply(reply, p.view)
		if len(reply.Threads) == 0 && before > 0 && !p.jsonOut && p.agent == "" && p.last == 0 {
			// An empty VIEW is not an empty board: say which view, and what else is there.
			fmt.Printf("AGENTS-BOARD | %d threads · %d posts\n\nthe recorder answered: nothing in the %s view "+
				"(%d other thread(s); --view all shows them)\n", reply.Stats.Threads, reply.Stats.Posts, p.view, before)
			return nil
		}
	}

	// --agent / --last are item-based and go through the shared Select, so
	// "issue-314's last 3" means the same here as in ##board copy.
	if p.agent != "" || p.last > 0 {
		out, err := renderBoardTailItems(reply, p)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
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

// runBoardTailFlow prints the board's flow model (design 052): JSON with
// --json — the same shape the golden fixture pins, so the TUI and the app can
// be compared against a live board — else a plain summary.
func runBoardTailFlow(store *session.Store, sess string, reply *board.QueryReply, p boardArgs) error {
	if p.agent != "" || p.last > 0 {
		return errors.New(progname.Rewrite("rysh board tail --view flow takes no --agent / --last: the flow is the whole board"))
	}
	panes, err := cli.BoardFlowPanes(store, sess)
	if err != nil {
		// The board answered; only names and the chain are missing. Say so
		// and carry on: ids are still a true picture of who talked to whom.
		fmt.Fprintf(os.Stderr, "rysh board tail: no pane snapshot (%v); labels fall back to personas, no chain\n", err)
	}
	f := board.BuildFlow(msg.NormalizeBoardID(p.board), reply.Threads, panes)
	if p.jsonOut {
		enc, merr := json.MarshalIndent(f, "", "  ")
		if merr != nil {
			return merr
		}
		fmt.Println(string(enc))
		return nil
	}
	fmt.Print(renderBoardFlow(f))
	return nil
}

// runBoardTailTeam is `rysh board tail --view team`: the board pane's t view —
// the fleet status table and the dependency tree — from a shell.
func runBoardTailTeam(store *session.Store, sess string, reply *board.QueryReply, p boardArgs) error {
	if p.agent != "" || p.last > 0 {
		return errors.New(progname.Rewrite("rysh board tail --view team takes no --agent / --last: the team is the whole fleet"))
	}
	panes, err := cli.BoardFlowPanes(store, sess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rysh board tail: no pane snapshot (%v); no fleet members to list\n", err)
	}
	f := board.BuildFlow(msg.NormalizeBoardID(p.board), reply.Threads, panes)
	if p.jsonOut {
		enc, merr := json.MarshalIndent(struct {
			Board        string          `json:"board"`
			Members      []board.TeamRow `json:"members"`
			Dependencies board.FlowDeps  `json:"dependencies"`
		}{f.Board, board.TeamRows(f), board.BuildFlowDeps(f)}, "", "  ")
		if merr != nil {
			return merr
		}
		fmt.Println(string(enc))
		return nil
	}
	members, blocked := board.TeamCounts(f)
	fmt.Printf("AGENTS-BOARD %s | team · %d members · %d blocked\n\n", f.Board, members, blocked)
	fmt.Println(strings.Join(board.RenderTeam(f, time.Now().UnixMilli(), 0), "\n"))
	return nil
}

// renderBoardFlow formats a flow model for a terminal: counts, edges, events.
func renderBoardFlow(f board.Flow) string {
	label := map[string]string{}
	for _, n := range f.Nodes {
		label[n.ID] = n.Label
	}
	name := func(id string) string {
		if l := label[id]; l != "" {
			return l
		}
		return id
	}
	var b strings.Builder
	s := f.Stats
	fmt.Fprintf(&b, "AGENTS-BOARD %s | flow · %d panes · %d prompts (%d answered · %d undelivered · %d open) · %d status\n",
		f.Board, s.Panes, s.Prompts, s.Answered, s.Undelivered, s.Open, s.Status)
	// The fleet status ledger (design 054), when the fleet actor has mirrored
	// one onto the panes: who is doing what, and who waits on whom.
	var status []string
	for _, n := range f.Nodes {
		if n.Kind != board.FlowNodePane || (n.Work == "" && n.Health == "") {
			continue
		}
		line := fmt.Sprintf("  %-20s %-9s %-8s %-12s %s", n.Label, n.Work, n.Activity, n.Health, n.Task)
		if len(n.BlockedBy) > 0 {
			names := make([]string, 0, len(n.BlockedBy))
			for _, id := range n.BlockedBy {
				names = append(names, name(id))
			}
			line += "  ⧗ " + strings.Join(names, ", ")
		}
		status = append(status, strings.TrimRight(line, " "))
	}
	if len(status) > 0 {
		b.WriteString("\nagents (work · activity · health · task)\n")
		b.WriteString(strings.Join(status, "\n") + "\n")
	}
	if deps := board.RenderFlowDeps(board.BuildFlowDeps(f)); len(deps) > 0 {
		b.WriteString("\n" + strings.Join(deps, "\n") + "\n")
	}
	b.WriteString("\nedges\n")
	for _, e := range f.Edges {
		line := fmt.Sprintf("  %-11s %s → %s  ×%d", e.Type, name(e.From), name(e.To), e.Count)
		if e.Type == board.FlowPrompt {
			line += fmt.Sprintf("  (%d answered · %d undelivered · %d open)", *e.Answered, *e.Undelivered, *e.Open)
			if e.LatencyMS != nil {
				line += fmt.Sprintf("  ack %dms", *e.LatencyMS)
			}
		}
		b.WriteString(line + "\n")
	}
	if len(f.Edges) == 0 {
		b.WriteString("  (none)\n")
	}
	b.WriteString("\nevents (oldest first)\n")
	for _, ev := range f.Events {
		id := ""
		if ev.CmdID != "" {
			id = " " + ev.CmdID
		}
		fmt.Fprintf(&b, "  %s  %-11s %s → %s%s  %s\n", time.UnixMilli(ev.TS).Format("15:04:05"),
			ev.Type, name(ev.From), name(ev.To), id, ev.Summary)
	}
	if len(f.Events) == 0 {
		b.WriteString("  the recorder answered: the board is empty\n")
	}
	return b.String()
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
			fmt.Fprintf(&b, "● %s%s  [%s]  %s%s\n",
				t.Root.Persona, boardTailAddress(t.Root), t.Root.Kind, boardTailClock(t.Root.TS), boardTailKey(t.Key))
			fmt.Fprintf(&b, "  %s\n", t.Root.Text)
		} else {
			// A provisional thread: replies arrived before their root, which is
			// expected rather than an error (design 025 §4.3). Say so instead
			// of rendering a reply as if it were a root.
			fmt.Fprintf(&b, "◌ (root has not arrived) %s\n", boardTailKey(t.Key))
		}
		for _, rep := range t.Replies {
			fmt.Fprintf(&b, "  ↳ %s%s  [%s]  %s\n", rep.Persona, boardTailAddress(rep), rep.Kind, boardTailClock(rep.TS))
			fmt.Fprintf(&b, "    %s\n", rep.Text)
		}
	}
	return b.String() + boardTailNotices(r)
}

// boardTailAddress shows where a design-050 command went, and which command an
// ack answers. Status posts render exactly as before.
func boardTailAddress(p *msg.MsgBoardPost) string {
	switch p.Kind {
	case msg.BoardKindCmd:
		return " → " + p.ToPersona + " · " + p.CmdID
	case msg.BoardKindAck:
		return " · " + p.CmdID
	}
	return ""
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

// renderBoardTailItems is tail with --agent / --last: the picked messages as
// the copy text, or — with --json — the recorder's reply narrowed to them, in
// the shape scripts already parse. Recorder answered and nothing matched is an
// ANSWER here (exit 0, and it says so), unlike copy, which refuses.
func renderBoardTailItems(reply *board.QueryReply, p boardArgs) (string, error) {
	picked, matched, _ := board.Select(board.Flatten(reply), p.agent, p.last)
	if p.jsonOut {
		enc, err := json.MarshalIndent(board.ItemsReply(reply, picked), "", "  ")
		return string(enc), err
	}
	if len(picked) == 0 {
		return "the recorder answered: " + strings.TrimPrefix(board.NoMessagesLine(p.agent), "[board] "), nil
	}
	out := board.FormatItems(picked)
	if len(picked) < matched {
		out += fmt.Sprintf("\n\n(last %d of %d messages)", len(picked), matched)
	}
	if notes := board.TruncationNotes(reply); notes != "" {
		out += "\n(window truncated: " + notes + ")"
	}
	return out, nil
}

// boardAllThreads asks for the whole board: copy and output count messages,
// and a thread window would silently drop some of "the last N".
const boardAllThreads = 0

// runBoardItems implements `rysh board copy|output` — the CLI twins of
// `##board copy|output`, over the same items, formatter and renderer.
func runBoardItems(cfg config.Config, p boardArgs) error {
	store, sess, err := resolveBoardSession(cfg, p.sess)
	if err != nil {
		return err
	}
	reply, err := cli.BoardTail(store, sess, p.board, board.Query{Limit: boardAllThreads})
	if err != nil {
		if errors.Is(err, board.ErrNoRecorder) {
			return boardTailNoRecorderError(err)
		}
		return err
	}
	out, err := boardItemsRun(reply, p, clipboard.WriteAll, board.WriteOutput)
	if out != "" {
		fmt.Println(out)
	}
	return err
}

// boardItemsRun is runBoardItems after the recorder answered. Pure apart from
// its two seams, so it is testable without a daemon, a clipboard or a browser.
func boardItemsRun(reply *board.QueryReply, p boardArgs, writeClip func(string) error,
	writeOut func(board.OutputRequest) (string, error)) (string, error) {
	a := p.items
	boardID := p.board
	if boardID == "" {
		boardID = msg.DefaultBoardID
	}
	if p.sub == "copy" {
		res, err := board.BuildCopy(reply, a.Worker, a.N)
		if err != nil {
			return "", err
		}
		if err := writeClip(res.Text); err != nil {
			return "", fmt.Errorf("[board] copy failed: the local clipboard: %w", err)
		}
		if p.print {
			// The text on stdout, the confirmation on stderr, so a pipe
			// carries only the messages.
			fmt.Fprintln(os.Stderr, res.Summary())
			return res.Text, nil
		}
		return res.Summary(), nil
	}

	picked, matched, total := board.Select(board.Flatten(reply), a.Worker, a.N)
	if len(picked) == 0 {
		return "", board.ErrNoMessages{Worker: a.Worker}
	}
	path, err := writeOut(board.OutputRequest{
		Page: board.HTMLPage{Board: boardID, Worker: a.Worker, N: a.N,
			Picked: picked, Matched: matched, Total: total, Now: time.Now()},
		Format: a.Format,
		Dir:    a.Out,
		Open:   !a.NoOpen,
	})
	if err != nil {
		return "", err
	}
	return board.OutputSummary(len(picked), matched, a.Worker, path), nil
}
