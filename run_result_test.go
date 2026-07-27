package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// runCollector — the Result's honest fact accumulation
// ---------------------------------------------------------------------------

// The collector must reflect a real session's observable stream: text output
// concatenated in order, bash commands from tool_start step events, tokens
// summed from usage-ledger records — and nothing invented for absent inputs.
func TestRunCollector_AccumulatesSessionFacts(t *testing.T) {
	c := newRunCollector(runBudget{})

	c.OnOutput(&msg.MsgAgenticOutput{Type: "text", Content: "The tests "})
	c.OnOutput(&msg.MsgAgenticOutput{Type: "thinking", Content: "hmm"})        // not answer text
	c.OnOutput(&msg.MsgAgenticOutput{Type: "tool_call", Content: "⏺ bash(x)"}) // decoration
	c.OnOutput(&msg.MsgAgenticOutput{Type: "text", Content: "pass."})

	c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolStart, Origin: "bash",
		Title: "bash: go test ./...", Category: "tool"})
	c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolEnd, Origin: "bash",
		Title: "bash: go test ./... — ✓"}) // tool_end must not double-record
	c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolStart, Origin: "file_read",
		Title: "file_read: main.go"}) // non-bash tools are not commands
	c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolStart, Origin: "bash",
		Title: "bash: git status"})

	if _, breached := c.OnUsage(&msg.MsgUsageRecord{InTokens: 900, OutTokens: 80,
		CacheRead: 15, CacheWrite: 5, CostMicroUSD: 1200}); breached {
		t.Fatal("no budget set — must never report a breach")
	}
	c.OnUsage(&msg.MsgUsageRecord{InTokens: 500, OutTokens: 100})

	if got := c.OutputText(); got != "The tests pass." {
		t.Fatalf("output = %q, want only the text stream in order", got)
	}
	if got, want := c.CommandLines(), []string{"go test ./...", "git status"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	if got := c.TokensUsed(); got != 1600 {
		t.Fatalf("tokens = %d, want 1600 (in+out+cache summed across records)", got)
	}
	if got := c.CostMicroUSD(); got != 1200 {
		t.Fatalf("cost = %d, want 1200", got)
	}
}

// Absent inputs stay absent: a provider with no usage records and a run with
// no bash calls yields zeros/empties, never fabricated values.
func TestRunCollector_AbsenceIsNotFabricated(t *testing.T) {
	c := newRunCollector(runBudget{})
	if c.TokensUsed() != 0 || c.OutputText() != "" || len(c.CommandLines()) != 0 {
		t.Fatalf("empty collector must report zero facts: %d %q %v",
			c.TokensUsed(), c.OutputText(), c.CommandLines())
	}
}

// A token budget breach is reported exactly once, on the record that crosses
// the ceiling.
func TestRunCollector_TokenBudgetBreachOnce(t *testing.T) {
	c := newRunCollector(runBudget{Tokens: 1000})
	if _, breached := c.OnUsage(&msg.MsgUsageRecord{InTokens: 600}); breached {
		t.Fatal("under ceiling — no breach yet")
	}
	detail, breached := c.OnUsage(&msg.MsgUsageRecord{InTokens: 600})
	if !breached {
		t.Fatal("1200 > 1000 tokens — must breach")
	}
	if detail == "" {
		t.Fatal("breach must carry a human-readable detail")
	}
	if _, again := c.OnUsage(&msg.MsgUsageRecord{InTokens: 10}); again {
		t.Fatal("breach must be reported exactly once")
	}
	// Spend keeps accumulating after the breach so the partial Result is honest.
	if c.TokensUsed() != 1210 {
		t.Fatalf("tokens = %d, want 1210", c.TokensUsed())
	}
}

// A USD budget uses the ledger's cost field.
func TestRunCollector_USDBudgetBreach(t *testing.T) {
	c := newRunCollector(runBudget{MicroUSD: 2_000_000}) // $2
	if _, breached := c.OnUsage(&msg.MsgUsageRecord{CostMicroUSD: 1_500_000}); breached {
		t.Fatal("$1.50 < $2 — no breach yet")
	}
	if _, breached := c.OnUsage(&msg.MsgUsageRecord{CostMicroUSD: 700_000}); !breached {
		t.Fatal("$2.20 > $2 — must breach")
	}
}

// Defensive: a step title without the "<origin>: " command prefix (format
// drift, or tools like bash_background that embed no command text) must be
// skipped, not recorded as a pseudo-command.
func TestRunCollector_UnparseableStepTitleSkipped(t *testing.T) {
	c := newRunCollector(runBudget{})
	c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolStart, Origin: "bash", Title: "bash"})
	if got := c.CommandLines(); len(got) != 0 {
		t.Fatalf("commands = %v, want none for a title with no command text", got)
	}
}

// ---------------------------------------------------------------------------
// git snapshots — files_changed derivation
// ---------------------------------------------------------------------------

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func writeT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The files_changed derivation must reflect what actually changed on disk
// during the run — modified tracked files and new untracked files — while
// excluding pre-existing dirt it cannot attribute.
func TestGitSnapshot_ChangedPathsReflectRealEdits(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	writeT(t, filepath.Join(dir, "tracked.go"), "package x\n")
	writeT(t, filepath.Join(dir, "dirty.txt"), "already dirty pre-run\n")
	gitT(t, dir, "add", "tracked.go")
	gitT(t, dir, "commit", "-q", "-m", "seed")
	// dirty.txt stays untracked — pre-existing dirt.

	before, ok := gitStatusSnapshot(dir)
	if !ok {
		t.Fatal("temp repo must snapshot")
	}

	// "The run" edits a tracked file and creates a new one.
	writeT(t, filepath.Join(dir, "tracked.go"), "package x // edited\n")
	writeT(t, filepath.Join(dir, "hello.txt"), "hello rysh\n")

	after, ok := gitStatusSnapshot(dir)
	if !ok {
		t.Fatal("snapshot after")
	}
	got := changedPaths(before, after)
	want := []string{"hello.txt", "tracked.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changedPaths = %v, want %v (pre-existing dirt excluded)", got, want)
	}
}

// Dirt that disappears during the run (an untracked file the agent deleted)
// is also a change.
func TestGitSnapshot_DeletedUntrackedFileIsAChange(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	writeT(t, filepath.Join(dir, "scratch.txt"), "temp\n")

	before, _ := gitStatusSnapshot(dir)
	if err := os.Remove(filepath.Join(dir, "scratch.txt")); err != nil {
		t.Fatal(err)
	}
	after, _ := gitStatusSnapshot(dir)

	if got := changedPaths(before, after); !reflect.DeepEqual(got, []string{"scratch.txt"}) {
		t.Fatalf("changedPaths = %v, want [scratch.txt]", got)
	}
}

// Outside a git repo the snapshot must report !ok so the Result's
// files_changed stays honestly empty instead of guessing.
func TestGitSnapshot_NonRepoReportsNotOK(t *testing.T) {
	if _, ok := gitStatusSnapshot(os.TempDir()); ok {
		t.Skip("system temp dir is unexpectedly inside a git repo")
	}
	dir := t.TempDir()
	if _, ok := gitStatusSnapshot(dir); ok {
		t.Fatalf("bare temp dir must not snapshot as a git repo")
	}
}
