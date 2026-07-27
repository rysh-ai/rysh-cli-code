package main

// Result derivation for `rysh run` (design 009): the runCollector accumulates
// the observable facts of a headless session — output text, executed bash
// commands, token/cost spend — and the git helpers below turn working-tree
// state into the Result's files_changed. Everything here is honestly derived
// from what the run actually published or did on disk; nothing is fabricated:
//
//   - Output: every "text" MsgAgenticOutput, in arrival order.
//   - Commands: bash tool invocations, read from StepToolStart step events
//     ("bash: <command>"). LIMITS (documented, not papered over): step titles
//     carry the command's first line truncated to 80 chars ("..." suffix), so
//     very long commands are recorded truncated; bash_background sessions emit
//     no command text in their step title and are therefore not recorded.
//   - TokensUsed / cost: summed from the session's MsgUsageRecord stream (the
//     design-003 usage ledger). A provider that reports no usage leaves the
//     count at 0 — absence is tolerated, never estimated.
//   - FilesChanged: the delta of `git status --porcelain` before vs after the
//     run. LIMITS: outside a git repo the field stays empty (a filesystem
//     walk is out of scope); a path already dirty before the run whose
//     porcelain status is unchanged after cannot be attributed to the run and
//     is excluded — use --worktree (fresh, clean tree) for exact attribution.

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// runCollector accumulates a run's observable facts. Safe for concurrent use:
// its inputs arrive on independent NATS subscription callbacks.
type runCollector struct {
	mu       sync.Mutex
	budget   runBudget
	breached bool
	output   strings.Builder
	commands []string
	tokens   int64
	costMuSD int64
}

func newRunCollector(budget runBudget) *runCollector {
	return &runCollector{budget: budget}
}

// OnOutput accumulates streamed "text" output into the Result's Output.
// Other output kinds (thinking, tool_call, tool_result, diff, error) are the
// transcript's decoration, not the agent's answer, and are skipped.
func (c *runCollector) OnOutput(m *msg.MsgAgenticOutput) {
	if m == nil || m.Type != "text" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.output.WriteString(m.Content)
}

// OnStep records executed bash commands from tool_start step events. The step
// title for the bash tool is "bash: <command first line, ≤80 chars>" (see
// rysh-shared/agentic stepTitle); the prefix is stripped so the Result carries
// the command text itself.
func (c *runCollector) OnStep(m *msg.MsgAgenticStep) {
	if m == nil || m.Kind != sharedmsg.StepToolStart || m.Origin != "bash" {
		return
	}
	cmd := strings.TrimPrefix(m.Title, m.Origin+": ")
	if cmd == "" || cmd == m.Title {
		// No embedded command text (defensive: title format drifted) — an
		// unparseable entry would grade meaninglessly, so skip rather than
		// record the raw title as if it were a command.
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commands = append(c.commands, cmd)
}

// OnUsage folds one usage-ledger record into the totals and reports whether
// this record crossed the --budget ceiling (only the FIRST crossing reports
// breached=true, so exactly one budget event reaches the decision loop).
func (c *runCollector) OnUsage(r *msg.MsgUsageRecord) (detail string, breachedNow bool) {
	if r == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens += int64(r.InTokens) + int64(r.OutTokens) + int64(r.CacheRead) + int64(r.CacheWrite)
	c.costMuSD += r.CostMicroUSD
	if c.breached || !c.budget.set() {
		return "", false
	}
	over := (c.budget.Tokens > 0 && c.tokens > c.budget.Tokens) ||
		(c.budget.MicroUSD > 0 && c.costMuSD > c.budget.MicroUSD)
	if !over {
		return "", false
	}
	c.breached = true
	return fmt.Sprintf("budget exhausted: spent %d tokens / $%.4f against a ceiling of %s",
		c.tokens, float64(c.costMuSD)/1e6, c.budget), true
}

// OutputText returns the accumulated agent answer text.
func (c *runCollector) OutputText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.output.String()
}

// CommandLines returns the executed bash commands in execution order.
func (c *runCollector) CommandLines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.commands...)
}

// TokensUsed returns the summed token count from the usage ledger (0 when the
// provider reported no usage).
func (c *runCollector) TokensUsed() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens
}

// CostMicroUSD returns the summed cost in micro-USD from the usage ledger.
func (c *runCollector) CostMicroUSD() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.costMuSD
}

// ---------------------------------------------------------------------------
// Working-tree snapshots (files_changed derivation)
// ---------------------------------------------------------------------------

// gitStatusSnapshot captures the working tree's dirty state as a map of
// path → porcelain status (modified, untracked, deleted, renamed …). ok is
// false when dir is not inside a git work tree (or git is unavailable), in
// which case files_changed cannot be honestly derived and stays empty.
func gitStatusSnapshot(dir string) (state map[string]string, ok bool) {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain", "-z").Output()
	if err != nil {
		return nil, false
	}
	state = map[string]string{}
	// -z format: "XY path\x00" per entry; renames/copies emit the origin path
	// as an extra NUL-separated field after the entry.
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		status, path := f[:2], f[3:]
		state[path] = status
		if status[0] == 'R' || status[0] == 'C' {
			if i+1 < len(fields) && fields[i+1] != "" {
				// The rename origin also changed (it went away).
				state[fields[i+1]] = status
				i++
			}
		}
	}
	return state, true
}

// changedPaths returns the paths whose git status differs between the before
// and after snapshots — new dirt, changed dirt kind, or dirt that disappeared
// (e.g. an untracked file the run deleted). Sorted for stable Results.
func changedPaths(before, after map[string]string) []string {
	seen := map[string]bool{}
	for path, st := range after {
		if before[path] != st {
			seen[path] = true
		}
	}
	for path := range before {
		if _, still := after[path]; !still {
			seen[path] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
