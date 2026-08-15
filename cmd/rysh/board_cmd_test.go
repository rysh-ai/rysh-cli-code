// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Both tests below pin defects found by the LIVE demo (design 025 §8a) that
// ~30 unit tests had not surfaced, because nothing exercised the CLI's own
// argument grammar.

// TestBoardReplyIsLabelledAsAReply: a reply used to be published with kind
// "milestone", because the CLI left Kind empty and NewBoardPost defaults an
// empty kind to milestone. msg.BoardKindReply existed and nothing set it — so
// Kind was wrong on exactly the messages threading exists to mark, and the
// board could not tell a reply from a root at a glance.
func TestBoardReplyIsLabelledAsAReply(t *testing.T) {
	got, err := parseBoardArgs([]string{"reply", "pane-a/1", "--as", "pane-b", "--", "store done"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.kind != msg.BoardKindReply {
		t.Errorf("reply kind = %q, want %q", got.kind, msg.BoardKindReply)
	}
	if got.threadID != "pane-a/1" {
		t.Errorf("threadID = %q, want pane-a/1", got.threadID)
	}
}

func TestBoardPostKeepsItsDefaultKind(t *testing.T) {
	// A post must NOT be labelled a reply. Empty here is correct: the daemon
	// defaults it to milestone, and hard-coding it in two places would let the
	// two defaults drift.
	got, err := parseBoardArgs([]string{"post", "--as", "pane-a", "--", "schema frozen"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.kind == msg.BoardKindReply {
		t.Errorf("a post must not be labelled a reply; kind = %q", got.kind)
	}
}

func TestBoardExplicitKindStillWins(t *testing.T) {
	got, err := parseBoardArgs([]string{"reply", "t/1", "--kind", "blocked", "--", "stuck"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.kind != "blocked" {
		t.Errorf("--kind must override the reply default; got %q", got.kind)
	}
}

// TestBoardReplyAcceptsFlagsInAnyOrder: `board reply --session s <thread> …`
// used to fail with "needs a thread id", because the thread id was read off the
// front before flags were extracted — while `board post` accepted its flags
// anywhere. Same tool, two grammars, and the usage text named neither.
func TestBoardReplyAcceptsFlagsInAnyOrder(t *testing.T) {
	for _, args := range [][]string{
		{"reply", "pane-a/1", "--session", "demo", "--as", "pane-b", "--", "after"},
		{"reply", "--session", "demo", "pane-a/1", "--as", "pane-b", "--", "before"},
		{"reply", "--as", "pane-b", "--session", "demo", "pane-a/1", "--", "both"},
	} {
		got, err := parseBoardArgs(args)
		if err != nil {
			t.Fatalf("parse(%v): %v", args, err)
		}
		if got.threadID != "pane-a/1" {
			t.Errorf("parse(%v): threadID = %q, want pane-a/1", args, got.threadID)
		}
		if got.sess != "demo" {
			t.Errorf("parse(%v): sess = %q, want demo", args, got.sess)
		}
		if got.as != "pane-b" {
			t.Errorf("parse(%v): as = %q, want pane-b", args, got.as)
		}
	}
}

// A thread id must never be lifted out of the message body: the text is
// verbatim after "--", so a reply whose text begins with a word must still
// require its own thread id.
func TestBoardReplyStillRequiresAThreadID(t *testing.T) {
	if _, err := parseBoardArgs([]string{"reply", "--as", "p", "--", "not-a-thread-id"}); err == nil {
		t.Fatal("a reply with no thread id must be refused, not take one from its text")
	} else if !strings.Contains(err.Error(), "needs a thread id") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// The usage line must document --session for reply. It did not, which is how
// the ordering defect stayed invisible: the flag worked, undocumented, in one
// position only.
func TestBoardUsageDocumentsSessionForReply(t *testing.T) {
	replyLine := ""
	for _, l := range strings.Split(boardUsage, "\n") {
		if strings.Contains(l, "board reply") {
			replyLine = l
		}
	}
	if replyLine == "" {
		t.Fatal("usage does not mention `board reply` at all")
	}
	if !strings.Contains(replyLine, "--session") {
		t.Errorf("reply usage omits --session, which it accepts: %q", replyLine)
	}
}

// TestThreadOpenerIsNotLabelledAReply pins a regression I introduced while
// fixing F-18 and a live board caught within minutes.
//
// Deriving kind from "does it carry a thread id" looks equivalent and is not:
// `board post --thread <id>` is how an agent OPENS a thread, so the thread's own
// ROOT carries a thread id. That version labelled the root a reply to itself.
// The rule is the subcommand.
func TestThreadOpenerIsNotLabelledAReply(t *testing.T) {
	got, err := parseBoardArgs([]string{"post", "--thread", "pane-a/1", "--as", "pane-b", "--", "opening a thread"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.kind == msg.BoardKindReply {
		t.Error("a thread OPENER carries a thread id but is the root, not a reply to itself")
	}
	if got.threadID != "pane-a/1" {
		t.Errorf("threadID = %q, want pane-a/1", got.threadID)
	}
}

func TestBoardPostWithNoThreadStaysARoot(t *testing.T) {
	got, err := parseBoardArgs([]string{"post", "--as", "pane-a", "--", "root"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.kind == msg.BoardKindReply {
		t.Errorf("a post with no thread must not be a reply; kind = %q", got.kind)
	}
}

// --- `rysh board tail` — the read path (design 027, work order W1-1) --------

// TestBoardTailAcceptsItsFlagsInAnyOrder is the F-19 rule applied to the new
// subcommand rather than rediscovered on it: a flag that works in one position
// only is how that defect stayed invisible for a whole epic.
func TestBoardTailAcceptsItsFlagsInAnyOrder(t *testing.T) {
	orders := [][]string{
		{"tail", "--session", "s", "--since", "1500", "--limit", "3", "--json"},
		{"tail", "--json", "--limit", "3", "--since", "1500", "--session", "s"},
		{"tail", "--limit", "3", "--json", "--session", "s", "--since", "1500"},
	}
	for _, args := range orders {
		got, err := parseBoardArgs(args)
		if err != nil {
			t.Fatalf("parseBoardArgs(%v): %v", args, err)
		}
		if got.sub != "tail" || got.sess != "s" || got.since != 1500 || got.limit != 3 || !got.jsonOut {
			t.Fatalf("parseBoardArgs(%v) = %+v", args, got)
		}
	}
}

// TestBoardTailRefusesAnUnparseableBound: a --limit the parser cannot read must
// be an error, not a silently unbounded board. The caller believed it asked for
// N threads; handing back all of them without saying so is the read path's
// version of answering a question that was never understood.
func TestBoardTailRefusesAnUnparseableBound(t *testing.T) {
	for _, args := range [][]string{
		{"tail", "--limit", "lots"},
		{"tail", "--limit", "-1"},
		{"tail", "--since", "yesterday"},
	} {
		if _, err := parseBoardArgs(args); err == nil {
			t.Fatalf("parseBoardArgs(%v) accepted an unreadable bound", args)
		}
	}
}

// TestBoardTailRefusesStrayPositionalArguments: tail takes no text, so a
// mistyped flag lands as a positional and must be refused rather than dropped.
func TestBoardTailRefusesStrayPositionalArguments(t *testing.T) {
	if _, err := parseBoardArgs([]string{"tail", "--limt", "5"}); err == nil {
		t.Fatal("parseBoardArgs accepted a typo'd flag as if it had been understood")
	}
	if _, err := parseBoardArgs([]string{"tail"}); err != nil {
		t.Fatalf("bare `board tail` should be legal: %v", err)
	}
}

// TestBoardUsageDocumentsTail: the same rule TestBoardUsageDocumentsSessionForReply
// pins for reply. An accepted flag that the usage line omits is a flag nobody
// finds.
func TestBoardUsageDocumentsTail(t *testing.T) {
	if !strings.Contains(boardUsage, "board tail") {
		t.Fatalf("usage does not mention `board tail`:\n%s", boardUsage)
	}
	for _, flag := range []string{"--since", "--limit", "--json"} {
		if !strings.Contains(boardUsage, flag) {
			t.Errorf("`board tail` accepts %s but the usage line does not document it:\n%s",
				flag, boardUsage)
		}
	}
}

// TestBoardTailRenderSaysTheRecorderAnswered is the rendering half of the
// F-20/F-23 guard, and it is the one that matters most on this surface.
//
// An empty board and an unreachable recorder are DIFFERENT FACTS, and the only
// thing separating them on a terminal is the words printed. runBoardTail
// refuses to print anything at all when nobody answered; this pins that the
// legitimately-empty case says so out loud, so the two can never be read as
// the same output.
func TestBoardTailRenderSaysTheRecorderAnswered(t *testing.T) {
	out := renderBoardTail(&board.QueryReply{Threads: []board.Thread{}})
	if !strings.Contains(out, "the recorder answered") {
		t.Fatalf("an empty board renders without saying anybody answered, so it is "+
			"indistinguishable from an unreachable recorder:\n%s", out)
	}
}

// TestBoardTailRenderDisclosesWhatItIsNotShowing: a bounded answer that does
// not name its bound presents a window as the whole board (design 025 §7.1a).
func TestBoardTailRenderDisclosesWhatItIsNotShowing(t *testing.T) {
	root := msg.NewBoardPost("pane-a", "planner", msg.BoardKindMilestone, "schema frozen", 1000)
	out := renderBoardTail(&board.QueryReply{
		Threads:  []board.Thread{{Key: "pane-a/1", Root: root}},
		Withheld: 4,
		Stats:    board.Stats{Threads: 5, Posts: 5},
	})
	if !strings.Contains(out, "schema frozen") {
		t.Fatalf("the post did not render:\n%s", out)
	}
	if !strings.Contains(out, "4 older threads not shown") {
		t.Fatalf("a truncated answer did not disclose its bound:\n%s", out)
	}
}

// TestBoardTailRenderMarksAThreadMissingItsRoot: a provisional thread must not
// render as though its first reply were the root (design 025 §4.3).
func TestBoardTailRenderMarksAThreadMissingItsRoot(t *testing.T) {
	reply := msg.NewBoardPost("pane-b", "builder", msg.BoardKindReply, "confirmed", 1001)
	out := renderBoardTail(&board.QueryReply{
		Threads: []board.Thread{{Key: "pane-a/1", Replies: []*msg.MsgBoardPost{reply}, Provisional: true}},
		Stats:   board.Stats{Threads: 1, Provisional: 1, Posts: 1},
	})
	if !strings.Contains(out, "root has not arrived") {
		t.Fatalf("a provisional thread rendered as a normal one:\n%s", out)
	}
}

// TestBoardTailNeverPrintsAnUnusableThreadKey: a standalone thread's key is
// minted behind a NUL byte so no agent can address it (design 025 §4.3a).
// Printing it offers the reader an address that cannot work AND emits a raw
// control character. Found by the live run, not by the ~40 board tests.
func TestBoardTailNeverPrintsAnUnusableThreadKey(t *testing.T) {
	root := msg.NewBoardPost("pane-b", "builder", msg.BoardKindMilestone, "unrelated milestone", 1000)
	out := renderBoardTail(&board.QueryReply{
		Threads: []board.Thread{{Key: "\x00standalone/3", Root: root}},
		Stats:   board.Stats{Threads: 1, Posts: 1},
	})
	if strings.ContainsRune(out, 0) {
		t.Fatalf("a NUL byte reached the terminal: %q", out)
	}
	if strings.Contains(out, "standalone/3") {
		t.Fatalf("printed a thread key no agent can reply to:\n%s", out)
	}
	if !strings.Contains(out, "no replies possible") {
		t.Fatalf("did not say why the thread has no address:\n%s", out)
	}
	// A real, mintable key must still be shown — it is how a reader replies.
	real := renderBoardTail(&board.QueryReply{
		Threads: []board.Thread{{Key: "pane-a/1", Root: root}},
		Stats:   board.Stats{Threads: 1, Posts: 1},
	})
	if !strings.Contains(real, "pane-a/1") {
		t.Fatalf("a usable thread key was hidden:\n%s", real)
	}
}

// TestBoardTailNoRecorderErrorSaysItIsNotAnEmptyBoard.
//
// This wording IS the safety argument on this surface. `rysh board tail` has
// exactly two ways to come back with nothing, and a reader has to be able to
// tell them apart: the recorder answered and the board is empty, or nobody
// answered and the board's contents are unknown. F-20 and F-23 were both the
// second state presented as the first.
func TestBoardTailNoRecorderErrorSaysItIsNotAnEmptyBoard(t *testing.T) {
	err := boardTailNoRecorderError(board.ErrNoRecorder)
	if err == nil {
		t.Fatal("no error produced")
	}
	if !errors.Is(err, board.ErrNoRecorder) {
		t.Fatalf("the error stopped wrapping ErrNoRecorder, so a caller can no longer "+
			"branch on it: %v", err)
	}
	if !strings.Contains(err.Error(), "NOT AN EMPTY BOARD") {
		t.Fatalf("the message does not distinguish an unreachable recorder from an "+
			"empty board:\n%s", err)
	}
	// And the empty-but-answered case must NOT borrow this wording, or the two
	// states collapse back into one.
	empty := renderBoardTail(&board.QueryReply{Threads: []board.Thread{}})
	if strings.Contains(empty, "NOT AN EMPTY BOARD") {
		t.Fatalf("a genuinely empty board is being reported as an unreachable recorder:\n%s", empty)
	}
}

// TestBoardTailAcceptsABoardFlag — found LIVE on 2026-08-11, not by a test.
//
// `--board` was extracted only on the post/reply path, below the early return
// that sends `tail` to its own parser. So `rysh board tail --board epic-07`
// reached that parser with the flag still in the argument list and died as
// "takes no positional arguments" — the read path advertising a flag in its own
// usage text and refusing it in practice.
//
// F-19 is the same rule (a flag that works in one position only), which is why
// the regression test asserts the flag from EVERY subcommand rather than just
// the one that broke.
func TestBoardTailAcceptsABoardFlag(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
	}{
		{"tail", []string{"tail", "--board", "epic-07"}},
		{"tail with other flags first", []string{"tail", "--limit", "5", "--board", "epic-07", "--json"}},
		{"tail with the flag first", []string{"tail", "--board", "epic-07", "--limit", "5"}},
		{"post", []string{"post", "--as", "p1", "--board", "epic-07", "--", "hello"}},
		{"reply", []string{"reply", "t/1", "--as", "p1", "--board", "epic-07", "--", "hi"}},
	} {
		got, err := parseBoardArgs(c.args)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.board != "epic-07" {
			t.Errorf("%s: board = %q, want epic-07", c.name, got.board)
		}
	}
}

// TestBoardTailAcceptsTheFleetSpelling — `--fleet` is the same flag under the
// name a fleet operator reaches for, and it has to work on the read path too.
func TestBoardTailAcceptsTheFleetSpelling(t *testing.T) {
	got, err := parseBoardArgs([]string{"tail", "--fleet", "epic-08"})
	if err != nil {
		t.Fatalf("tail --fleet: %v", err)
	}
	if got.board != "epic-08" {
		t.Fatalf("board = %q, want epic-08", got.board)
	}
}

// TestBoardTailStillRefusesARealPositional — the hoisted flag must not have
// turned the leftover check into a rubber stamp.
func TestBoardTailStillRefusesARealPositional(t *testing.T) {
	if _, err := parseBoardArgs([]string{"tail", "epic-07"}); err == nil {
		t.Fatal("a bare positional was accepted by board tail")
	}
	if _, err := parseBoardArgs([]string{"tail", "--board", "epic.07"}); err == nil {
		t.Fatal("an illegal board id was accepted")
	}
}
