package main

import (
	"strings"
	"testing"

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
