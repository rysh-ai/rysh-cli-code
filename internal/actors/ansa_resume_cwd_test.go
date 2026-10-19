// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `##ansa resume` brought a 42-agent codex fleet back into ONE conversation.
//
// A restarted daemon restores every pane as a fresh shell at the session root.
// The resume line was delivered into that shell with no `cd`, and
// `codex resume --last` is filtered to the current directory — so every agent
// resolved to the same newest root conversation. One pane got it; every other
// showed "this conversation is open in another session". `fleetctl verify`
// still read them all as running, because codex stays up while it says so.
//
// The native path (`##codex`, design 029) already knew this and cds from
// agent.cwd; the fleet path did not. claude is exposed the same way — it cannot
// find a session stored under a directory it is not in — so this is not a
// codex-only fix.

func fleetAgentPane(id, agent, worktree string) ansaPane {
	return ansaPane{ID: id, GivenName: id, Meta: map[string]string{
		"fleet.role":     "worker",
		"fleet.agent":    agent,
		"fleet.worktree": worktree,
		"agent.args":     "--dangerously-bypass-approvals-and-sandbox",
	}}
}

func mustResumeLine(t *testing.T, p ansaPane, agent, sid string) string {
	t.Helper()
	line, err := ansaResumeLine(p, agent, sid)
	if err != nil {
		t.Fatalf("resume line for %s: %v", p.ID, err)
	}
	return line
}

// THE BUG, as a test: two codex agents in two worktrees must get two
// different lines, each standing in its own directory. Before the fix both got
// the identical bare `codex resume --last …`, i.e. the same conversation.
func TestTwoCodexFleetPanesResumeIntoTheirOwnWorktrees(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "wt-a"), filepath.Join(root, "wt-b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	la := mustResumeLine(t, fleetAgentPane("pane-a", "codex", a), "codex", "")
	lb := mustResumeLine(t, fleetAgentPane("pane-b", "codex", b), "codex", "")

	if la == lb {
		t.Fatalf("two agents in two worktrees got the SAME resume line — they will both "+
			"take whichever conversation is newest where the shell stands:\n  %q", la)
	}
	for _, c := range []struct{ line, dir string }{{la, a}, {lb, b}} {
		cd := "cd " + shellQuote(c.dir) + " && "
		want := cd + "codex resume --last"
		if !strings.HasPrefix(c.line, cd) || !strings.HasPrefix(cd+agentCommandOf(strings.TrimPrefix(c.line, cd)), want) {
			t.Errorf("resume must cd into the agent's own worktree first:\n  got  %q\n  want prefix %q", c.line, want)
		}
	}
}

// claude has the same exposure: `claude --resume <id>` looks the session up
// under the current directory.
func TestAClaudeFleetPaneAlsoResumesInItsWorktree(t *testing.T) {
	wt := t.TempDir()
	line := mustResumeLine(t, fleetAgentPane("pane-c", "claude", wt), "claude", "pane-c")
	cd := "cd " + shellQuote(wt) + " && "
	want := cd + "claude --resume pane-c"
	if !strings.HasPrefix(line, cd) || !strings.HasPrefix(cd+agentCommandOf(strings.TrimPrefix(line, cd)), want) {
		t.Errorf("got %q, want prefix %q", line, want)
	}
}

// fleet.worktree WINS over agent.cwd on a fleet pane — the reverse was the
// first version of this fix, and the live fleet proved it wrong on 2026-09-21.
// video-roadmap carried agent.native=1 and agent.cwd=<repo root>, left behind
// by an ad-hoc `##codex` run in that pane, while fleet.worktree was correct.
// agent.cwd is a snapshot taken when `##codex` ran; fleet.worktree is the
// directory fleetctl assigned and the standing orders require the agent to
// stay in. Preferring the snapshot resumed that agent in the root, onto the
// shared conversation — the very bug this fix exists for.
func TestTheFleetWorktreeWinsOverAStaleAgentCwd(t *testing.T) {
	wt, stale := t.TempDir(), t.TempDir() // stale stands in for the repo root
	p := fleetAgentPane("pane-d", "codex", wt)
	p.Meta["agent.cwd"] = stale
	p.Meta["agent.native"] = "1"
	line := mustResumeLine(t, p, "codex", "")
	if !strings.HasPrefix(line, "cd "+shellQuote(wt)+" && ") {
		t.Errorf("fleet.worktree must win over a stale agent.cwd:\n  got %q\n  want cd into %s", line, wt)
	}
}

// With no worktree recorded — a fleet built without --worktrees — agent.cwd is
// still better than nothing: it is the only directory anyone wrote down.
func TestAgentCwdIsTheFallbackWhenNoWorktreeIsRecorded(t *testing.T) {
	cwd := t.TempDir()
	p := fleetAgentPane("pane-h", "codex", "")
	p.Meta["agent.cwd"] = cwd
	line := mustResumeLine(t, p, "codex", "")
	if !strings.HasPrefix(line, "cd "+shellQuote(cwd)+" && ") {
		t.Errorf("with no worktree, agent.cwd should be used: %q", line)
	}
}

// A pane that recorded no directory keeps the old behaviour exactly: no cd,
// resume where the shell stands. Nothing to cd to is not a reason to refuse.
func TestAPaneWithNoRecordedDirectoryResumesWhereItStands(t *testing.T) {
	p := fleetAgentPane("pane-e", "codex", "")
	line := mustResumeLine(t, p, "codex", "")
	if line != ansaResumeCommand("codex", "", ansaPaneAgentArgs(p)) {
		t.Errorf("with no directory recorded the line must be unchanged: %q", line)
	}
}

// A worktree that is gone must REFUSE, not fall back to resuming in place:
// resuming in place is precisely how an agent attaches to someone else's
// conversation, and the receipt would still say "resumed".
func TestAMissingWorktreeRefusesRatherThanResumingTheWrongConversation(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "removed-worktree")
	line, err := ansaResumeLine(fleetAgentPane("pane-f", "codex", gone), "codex", "")
	if err == nil {
		t.Fatalf("a missing worktree must be an error, got line %q", line)
	}
	if line != "" {
		t.Errorf("an error must come with no line to deliver, got %q", line)
	}
	if !strings.Contains(err.Error(), gone) {
		t.Errorf("the error should name the directory: %v", err)
	}
}

// Worktree paths are user-chosen; a quote in one must not break the line.
func TestAWorktreePathWithAQuoteIsShellQuoted(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "it's a tree")
	if err := os.Mkdir(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	line := mustResumeLine(t, fleetAgentPane("pane-g", "codex", wt), "codex", "")
	if !strings.HasPrefix(line, "cd "+shellQuote(wt)+" && ") {
		t.Errorf("path not quoted as a single word: %q", line)
	}
}
