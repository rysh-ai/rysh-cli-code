package main

// `rysh run` — CI mode / headless one-shot execution (design 009, roadmap B2).
//
// Boots a THROWAWAY detached session daemon (unique name, never an existing
// session), submits the prompt to the active pane in agentic prompt mode,
// streams the agent's text output to stdout, and exits with a code that a CI
// pipeline can gate on. Fail-closed: there is no human at the keyboard, so an
// approval request is a terminal failure (gate-blocked), not something to
// wait on.
//
// The prompt is either a bare string (`rysh run "fix the test"`) or a skill
// file (`rysh run reviewer.md [--prompt <task>]`, design 009 §3.1) whose
// frontmatter is parsed exactly like ##agent skill files (internal/actors);
// `isolation: worktree` in the frontmatter implies --worktree.
//
// Exit codes (design 009 §3.1 table):
//	0  terminal phase "done"
//	1  error (terminal phase "error", cancelled, daemon boot failure,
//	   event stream lost)
//	2  partial saved — the run paused resumable (max duration / context
//	   budget / iterations) with nobody to resume it; design: "resume"
//	3  gate-blocked — an approval gate fired (pane.*.approval.request)
//	4  budget-exhausted — the --budget ceiling was crossed
//	5  --timeout exceeded before any terminal phase (not in the design
//	   table; kept distinct so CI can tell a hung run from a blocked one)
//
// Alongside the exit code the run produces an eval.Result (--result-out and
// the --json line) honestly derived from the session: files changed (git
// working-tree diff before/after), commands executed (bash tool step events),
// final output text, and token spend (usage ledger records). See
// run_result.go for the derivations and their documented limits.
// --json-audit additionally writes the full audit artifact (tool calls,
// approvals + policy citations, diffs, SNAT observability, budget, usage) —
// see run_audit.go for what is derivable from the bus and what is documented
// absent.
//
// Record/replay (design 009 §3.2 deterministic mode): --record <dir> saves
// the provider interaction transcript; --replay <dir> re-runs against the
// recorded turns via the replay provider (no key, no token spend). The
// directory reaches the daemon child through RYSH_RECORD_DIR/RYSH_REPLAY_DIR
// (same road as the --provider override).
//
// Future (design 009, deliberately not built here): the GitHub Action/App
// (§3.3) and `rysh eval --update-baseline`.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/actors"
	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/eval"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
	"github.com/rysh-ai/rysh-cli-code/internal/worktree"
)

// runDefaultTimeout bounds the whole run (daemon boot + agentic loop) when
// --timeout is not given.
const runDefaultTimeout = 10 * time.Minute

// runBootTimeout bounds daemon spawn + first workspace snapshot. Kept separate
// from --timeout so a tiny --timeout still gives the daemon a chance to report
// a boot failure rather than always reading as a timeout.
const runBootTimeout = 20 * time.Second

// Exit codes for `rysh run` (design 009 §3.1; see the file header).
const (
	runExitDone        = 0
	runExitError       = 1
	runExitPartial     = 2
	runExitGateBlocked = 3
	runExitBudget      = 4
	runExitTimeout     = 5
)

// runOptions is the parsed `rysh run` invocation.
type runOptions struct {
	Prompt    string // text submitted to the pane (bare prompt or skill body)
	TaskArg   string // --prompt: task text appended to a skill body
	SkillPath string // set when the positional arg was an existing .md file
	SkillName string // frontmatter name (or derived) of the loaded skill
	Timeout   time.Duration
	JSON      bool
	Keep      bool
	Provider  string    // --provider: override the configured provider for this run
	Worktree  bool      // --worktree: run inside a fresh git worktree
	Budget    runBudget // --budget: token or USD ceiling (zero value ⇒ none)
	ResultOut string    // --result-out: write the eval.Result JSON here
	JSONAudit string    // --json-audit: write the full audit artifact here
	RecordDir string    // --record: save the provider transcript to this dir
	ReplayDir string    // --replay: serve provider turns from this recorded dir
}

// runBudget is a spend ceiling: exactly one of Tokens / MicroUSD is set.
// The zero value means "no budget".
type runBudget struct {
	Tokens   int64 // > 0 ⇒ total-token ceiling
	MicroUSD int64 // > 0 ⇒ cost ceiling in micro-USD
}

func (b runBudget) set() bool { return b.Tokens > 0 || b.MicroUSD > 0 }

// String renders the ceiling for error/detail messages.
func (b runBudget) String() string {
	switch {
	case b.Tokens > 0:
		return fmt.Sprintf("%d tokens", b.Tokens)
	case b.MicroUSD > 0:
		return fmt.Sprintf("$%.2f", float64(b.MicroUSD)/1e6)
	}
	return "none"
}

// parseBudget parses a --budget value. Two spellings:
//
//	$2, $2.50, 2usd, 2.5usd  → a USD ceiling
//	150000, 150k, 2m         → a total-token ceiling
//
// The design's flag sketch (`--budget 2books`) names no unit, so both honest
// interpretations are accepted and disambiguated by spelling.
func parseBudget(s string) (runBudget, error) {
	orig := s
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return runBudget{}, errors.New("--budget requires a value (e.g. --budget $2 or --budget 150000)")
	}
	usd := false
	if strings.HasPrefix(s, "$") {
		usd = true
		s = strings.TrimPrefix(s, "$")
	}
	if strings.HasSuffix(s, "usd") {
		usd = true
		s = strings.TrimSpace(strings.TrimSuffix(s, "usd"))
	}
	if usd {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v <= 0 {
			return runBudget{}, fmt.Errorf("invalid --budget %q (want a positive dollar amount, e.g. $2.50)", orig)
		}
		return runBudget{MicroUSD: int64(v * 1e6)}, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1_000, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "m")
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return runBudget{}, fmt.Errorf("invalid --budget %q (want tokens like 150000/150k, or dollars like $2)", orig)
	}
	return runBudget{Tokens: v * mult}, nil
}

const runUsage = `usage: rysh run [flags] "<prompt>" | <skill.md>
  --prompt <p>       task text (with a skill file: appended to the skill body)
  --timeout <dur>    overall deadline (default 10m)
  --provider <name>  override the configured provider for this run
  --budget <b>       spend ceiling: $2 / 2usd (cost) or 150000 / 150k (tokens)
  --worktree         run inside a fresh git worktree (removed unless --keep)
  --result-out <f>   write the eval.Result JSON artifact to <f>
  --json-audit <f>   write the full audit artifact (tool calls, approvals,
                     diffs, SNAT, budget, usage) to <f>
  --record <dir>     save the provider interaction transcript to <dir>
  --replay <dir>     re-run against the transcript recorded in <dir>
                     (deterministic; no API key or token spend)
  --json             print a machine-readable result line to stdout
  --keep             keep the throwaway session (and worktree) after the run`

// parseRunArgs parses `rysh run` flags. Pure (no file I/O) so the flag
// surface is testable without a session; skill-file resolution happens in
// loadRunPrompt. Positional args are joined into the prompt so both
// `rysh run "do x"` and `rysh run do x` work.
func parseRunArgs(args []string) (runOptions, error) {
	opts := runOptions{Timeout: runDefaultTimeout}
	var positional []string
	strFlag := func(name string, i *int) (string, error) {
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s requires a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--timeout":
			v, err := strFlag(a, &i)
			if err != nil {
				return opts, errors.New("--timeout requires a duration (e.g. --timeout 5m)")
			}
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return opts, fmt.Errorf("invalid --timeout %q (want a positive Go duration, e.g. 90s, 10m)", v)
			}
			opts.Timeout = d
		case "--prompt":
			v, err := strFlag(a, &i)
			if err != nil {
				return opts, err
			}
			opts.TaskArg = v
		case "--provider":
			v, err := strFlag(a, &i)
			if err != nil {
				return opts, err
			}
			if !provider.IsKnownProviderName(v) {
				return opts, fmt.Errorf("unknown --provider %q (known: %s)",
					v, strings.Join(provider.KnownProviderNames(), ", "))
			}
			opts.Provider = v
		case "--budget":
			v, err := strFlag(a, &i)
			if err != nil {
				return opts, errors.New("--budget requires a value (e.g. --budget $2 or --budget 150000)")
			}
			b, err := parseBudget(v)
			if err != nil {
				return opts, err
			}
			opts.Budget = b
		case "--result-out":
			v, err := strFlag(a, &i)
			if err != nil {
				return opts, err
			}
			opts.ResultOut = v
		case "--json-audit":
			v, err := strFlag(a, &i)
			if err != nil {
				return opts, err
			}
			opts.JSONAudit = v
		case "--record":
			v, err := strFlag(a, &i)
			if err != nil {
				return opts, err
			}
			opts.RecordDir = v
		case "--replay":
			v, err := strFlag(a, &i)
			if err != nil {
				return opts, err
			}
			opts.ReplayDir = v
		case "--worktree":
			opts.Worktree = true
		case "--json":
			opts.JSON = true
		case "--keep":
			opts.Keep = true
		default:
			if strings.HasPrefix(a, "-") {
				return opts, fmt.Errorf(progname.Rewrite("unknown rysh run flag %q"), a)
			}
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 && opts.TaskArg == "" {
		return opts, errors.New(progname.Rewrite(runUsage))
	}
	if opts.RecordDir != "" && opts.ReplayDir != "" {
		return opts, errors.New("--record and --replay are mutually exclusive (a replay serves recorded turns; there is nothing new to record)")
	}
	opts.Prompt = strings.Join(positional, " ")
	return opts, nil
}

// loadRunPrompt resolves the skill-file form of `rysh run` (design 009 §3.1):
// when the positional argument is a path to an existing .md file, the file is
// parsed exactly like an ##agent skill file (frontmatter + body, ${VAR}
// expansion — the internal/actors seam) and its body becomes the prompt;
// --prompt text is appended as the task. `isolation: worktree` in the
// frontmatter turns on --worktree. Any other argument shape stays a bare
// prompt (backward compatible). Mutates opts in place.
func loadRunPrompt(opts *runOptions) error {
	arg := strings.TrimSpace(opts.Prompt)
	if strings.HasSuffix(arg, ".md") && !strings.ContainsRune(arg, '\n') {
		if fi, err := os.Stat(arg); err == nil && fi.Mode().IsRegular() {
			sk, err := actors.ParseRunSkillFile(arg)
			if err != nil {
				return fmt.Errorf("run: load skill file %s: %w", arg, err)
			}
			if sk.Prompt == "" {
				return fmt.Errorf("run: skill file %s has an empty body", arg)
			}
			opts.SkillPath = arg
			opts.SkillName = sk.Name
			opts.Prompt = sk.Prompt
			if sk.Isolation == "worktree" {
				opts.Worktree = true
			}
		}
	}
	if opts.TaskArg != "" {
		if opts.Prompt == "" {
			opts.Prompt = opts.TaskArg
		} else {
			opts.Prompt = opts.Prompt + "\n\n" + opts.TaskArg
		}
	}
	if strings.TrimSpace(opts.Prompt) == "" {
		return errors.New(progname.Rewrite(runUsage))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Exit-semantics core (pure, channel-fed, unit-tested)
// ---------------------------------------------------------------------------

type runEventKind int

const (
	runEvPhase    runEventKind = iota // MsgAgenticStatus.Phase observed
	runEvApproval                     // MsgApprovalRequest observed — fail closed
	runEvBudget                       // --budget ceiling crossed — stop the run
)

// runEvent is one decision-relevant observation from the session's bus.
// Output streaming is NOT an event: it is printed as it arrives and never
// influences the exit code.
type runEvent struct {
	Kind   runEventKind
	Phase  string // runEvPhase: MsgAgenticStatus phase string
	Detail string // human-readable context (approval description, error text)
}

// runOutcome is the terminal result of a run.
type runOutcome struct {
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Detail   string `json:"-"`
}

// decideRunOutcome consumes bus events until one decides the run. This is the
// design's heart and is deliberately a pure function of two channels so the
// full exit-code matrix is testable with synthetic events (no daemon, no LLM):
//
//   - approval request → gate-blocked (exit 3) BEFORE any later phase can be
//     read: CI has no human, so a gate that would block interactively fails
//     the run closed
//   - budget event → budget-exhausted (exit 4), same preemption logic
//   - phase "done" → exit 0; "error" → exit 1
//   - "paused" → exit 2: the orchestrator saved a resumable PARTIAL (max
//     duration / context budget / iterations) and asks for "continue" —
//     the design's resume-needed code, reported as such to CI
//   - "cancelled" → exit 1: interrupted, nothing to gate on
//   - timeout fires → exit 5
//   - events channel closes (daemon/NATS gone) → exit 1
//
// Non-terminal phases (planning, executing, grounding, evaluating, compacting,
// waiting_approval) keep the loop waiting — waiting_approval alone does not
// decide, because the paired MsgApprovalRequest carries the description worth
// reporting and auto-approved gates never surface a request.
func decideRunOutcome(events <-chan runEvent, timeout <-chan time.Time) runOutcome {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return runOutcome{Status: "disconnected", ExitCode: runExitError,
					Detail: "event stream closed before a terminal phase (daemon or NATS connection lost)"}
			}
			switch ev.Kind {
			case runEvApproval:
				return runOutcome{Status: "gate_blocked", ExitCode: runExitGateBlocked, Detail: ev.Detail}
			case runEvBudget:
				return runOutcome{Status: "budget_exhausted", ExitCode: runExitBudget, Detail: ev.Detail}
			case runEvPhase:
				switch ev.Phase {
				case "done":
					return runOutcome{Status: "done", ExitCode: runExitDone}
				case "error":
					return runOutcome{Status: "error", ExitCode: runExitError, Detail: ev.Detail}
				case "paused":
					return runOutcome{Status: "paused", ExitCode: runExitPartial,
						Detail: "run paused with a resumable partial and no interactive operator to resume it"}
				case "cancelled":
					return runOutcome{Status: "cancelled", ExitCode: runExitError,
						Detail: "run cancelled with no interactive operator to resume it"}
				}
			}
		case <-timeout:
			return runOutcome{Status: "timeout", ExitCode: runExitTimeout}
		}
	}
}

// runResultJSON renders the machine-consumable final line for --json. The
// embedded result is the same eval.Result written by --result-out, so a CI
// step can grade straight from stdout.
func runResultJSON(o runOutcome, duration time.Duration, sessionName string, res eval.Result) string {
	b, err := json.Marshal(struct {
		Status     string      `json:"status"`
		ExitCode   int         `json:"exit_code"`
		DurationMs int64       `json:"duration_ms"`
		Session    string      `json:"session"`
		Result     eval.Result `json:"result"`
	}{o.Status, o.ExitCode, duration.Milliseconds(), sessionName, res})
	if err != nil {
		// The struct above cannot fail to marshal; guard stays for honesty.
		return fmt.Sprintf(`{"status":"error","exit_code":1,"duration_ms":%d,"session":%q}`,
			duration.Milliseconds(), sessionName)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Command entry
// ---------------------------------------------------------------------------

// runRunCmd implements `rysh run`. It returns the process exit code; err (when
// non-nil) is a message for stderr on top of that code. All cleanup happens
// before return so the caller may os.Exit immediately.
func runRunCmd(cfg config.Config, configPath string, args []string) (int, error) {
	opts, err := parseRunArgs(args)
	if err != nil {
		return runExitError, err
	}
	if err := loadRunPrompt(&opts); err != nil {
		return runExitError, err
	}
	if opts.SkillPath != "" {
		fmt.Fprintf(os.Stderr, "[run] skill %s loaded from %s\n", opts.SkillName, opts.SkillPath)
	}

	// --provider: export the override and re-derive the config so the
	// provider-specific key env vars (e.g. GEMINI_API_KEY) apply, exactly as
	// if RYSH_PROVIDER had been set. The throwaway daemon (a child process
	// that reads its own config) inherits the env, so the override reaches it.
	if opts.Provider != "" {
		if err := os.Setenv("RYSH_PROVIDER", opts.Provider); err != nil {
			return runExitError, fmt.Errorf("run: set RYSH_PROVIDER: %w", err)
		}
		cfg = config.LoadFrom(configPath)
	}

	start := time.Now()
	outcome, res, audit, sessionName, err := executeHeadlessRun(cfg, configPath, opts)
	if err != nil {
		return runExitError, err
	}
	duration := time.Since(start)

	// The Result artifact is written even for gate-blocked / budget-exhausted
	// / timed-out runs: an honest partial is exactly what the eval harness
	// grades against.
	if opts.ResultOut != "" {
		if werr := writeRunResult(opts.ResultOut, res); werr != nil {
			fmt.Fprintf(os.Stderr, "[run] WARNING: write --result-out: %v\n", werr)
		}
	}
	// Same policy for the audit artifact: a blocked or exhausted run is
	// exactly when the audit trail matters most.
	if opts.JSONAudit != "" && audit != nil {
		if werr := writeRunAudit(opts.JSONAudit, audit); werr != nil {
			fmt.Fprintf(os.Stderr, "[run] WARNING: write --json-audit: %v\n", werr)
		}
	}

	if opts.JSON {
		fmt.Println(runResultJSON(outcome, duration, sessionName, res))
	} else {
		detail := ""
		if outcome.Detail != "" {
			detail = " — " + outcome.Detail
		}
		fmt.Fprintf(os.Stderr, "[run] %s (exit %d, %s)%s\n",
			outcome.Status, outcome.ExitCode, duration.Round(time.Millisecond), detail)
	}
	return outcome.ExitCode, nil
}

// writeRunResult writes the eval.Result artifact as indented JSON.
func writeRunResult(path string, res eval.Result) error {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// executeHeadlessRun performs one full headless run: provider gate, optional
// worktree isolation, throwaway daemon boot, prompt execution, and honest
// Result + audit derivation. It is the shared engine behind `rysh run` and
// `rysh eval --live` (called in-process — eval never shells out to rysh).
//
// The returned error means the run could not be attempted (bad provider, no
// worktree possible, daemon boot failure); once the prompt is submitted all
// failures are expressed through the outcome instead. The returned audit is
// non-nil whenever the run was attempted.
func executeHeadlessRun(cfg config.Config, configPath string, opts runOptions) (runOutcome, eval.Result, *runAudit, string, error) {
	var res eval.Result

	// --record / --replay: the transcript directory travels to the daemon
	// child through the environment (the same road as the --provider
	// override — spawnDaemon's child inherits it). Absolute paths: --worktree
	// chdirs before boot, and the daemon resolves its own cwd.
	providerName := cfg.ProviderName
	if opts.ReplayDir != "" {
		abs, err := filepath.Abs(opts.ReplayDir)
		if err != nil {
			return runOutcome{}, res, nil, "", fmt.Errorf("run: resolve --replay dir: %w", err)
		}
		if _, err := os.Stat(filepath.Join(abs, provider.TranscriptFileName)); err != nil {
			return runOutcome{}, res, nil, "", fmt.Errorf(
				progname.Rewrite("run: --replay %s holds no %s (record one with `rysh run --record %s`): %w"),
				opts.ReplayDir, provider.TranscriptFileName, opts.ReplayDir, err)
		}
		if err := os.Setenv(provider.ReplayDirEnv, abs); err != nil {
			return runOutcome{}, res, nil, "", fmt.Errorf("run: set %s: %w", provider.ReplayDirEnv, err)
		}
		defer func() { _ = os.Unsetenv(provider.ReplayDirEnv) }()
		providerName = "replay"
		fmt.Fprintf(os.Stderr, "[run] replaying recorded transcript from %s (no live provider)\n", abs)
	}
	if opts.RecordDir != "" {
		abs, err := filepath.Abs(opts.RecordDir)
		if err != nil {
			return runOutcome{}, res, nil, "", fmt.Errorf("run: resolve --record dir: %w", err)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return runOutcome{}, res, nil, "", fmt.Errorf("run: create --record dir: %w", err)
		}
		if err := os.Setenv(provider.RecordDirEnv, abs); err != nil {
			return runOutcome{}, res, nil, "", fmt.Errorf("run: set %s: %w", provider.RecordDirEnv, err)
		}
		defer func() { _ = os.Unsetenv(provider.RecordDirEnv) }()
		fmt.Fprintf(os.Stderr, "[run] recording provider transcript to %s\n", abs)
	}

	// Fail closed BEFORE booting anything: with no usable provider the daemon
	// silently falls back to a mock whose canned end_turn reply reads as a
	// successful "done" run — a no-op green in CI. Refusing here turns a
	// misconfigured pipeline into a loud exit 1 instead. (Replay counts as
	// usable: the transcript is the provider, no key needed.)
	if !agentic.ProviderUsable(cfg) {
		return runOutcome{}, res, nil, "", fmt.Errorf("run: no usable agentic provider (provider %q requires an API key; "+
			"set RYSH_API_KEY or api_key in rysh.config.yaml) — refusing a headless run against the mock provider",
			cfg.ProviderName)
	}

	start := time.Now()
	// Unique throwaway name: never collides with a user's session, so cleanup
	// can never touch anyone else's daemon.
	sessionName := fmt.Sprintf("run-%d", start.UnixNano())
	cfg.SessionName = sessionName

	// --worktree: run inside a fresh git worktree (design 008 isolation) so
	// every file change is attributable to this run. The daemon inherits the
	// process cwd (spawnDaemon), so chdir before boot and restore after.
	runDir, err := os.Getwd()
	if err != nil {
		return runOutcome{}, res, nil, "", fmt.Errorf("run: getwd: %w", err)
	}
	if opts.Worktree {
		if !worktree.IsGitRepo(runDir) {
			return runOutcome{}, res, nil, "", errors.New("run: --worktree requires running inside a git repository")
		}
		root, rerr := worktree.RepoRoot(runDir)
		if rerr != nil {
			return runOutcome{}, res, nil, "", fmt.Errorf("run: resolve repo root: %w", rerr)
		}
		wtPath := worktree.Dir(root, sessionName)
		if werr := worktree.Add(root, wtPath, sessionName); werr != nil {
			return runOutcome{}, res, nil, "", fmt.Errorf("run: create worktree: %w", werr)
		}
		if cerr := os.Chdir(wtPath); cerr != nil {
			_ = worktree.Remove(root, wtPath, true)
			_ = worktree.DeleteBranch(root, sessionName)
			return runOutcome{}, res, nil, "", fmt.Errorf("run: enter worktree: %w", cerr)
		}
		fmt.Fprintf(os.Stderr, "[run] worktree %s (branch %s)\n", wtPath, sessionName)
		prevDir := runDir
		runDir = wtPath
		defer func() {
			_ = os.Chdir(prevDir)
			if opts.Keep {
				fmt.Fprintf(os.Stderr, "[run] --keep: worktree %s left in place\n", wtPath)
				return
			}
			// The graded artifact is the Result, computed before this point;
			// the worktree itself is disposable, changes included.
			_ = worktree.Remove(root, wtPath, true)
			_ = worktree.DeleteBranch(root, sessionName)
		}()
	}

	// Working-tree snapshot BEFORE the agent runs: FilesChanged is the honest
	// delta of git state across the run (see gitStatusSnapshot for limits).
	before, inRepo := gitStatusSnapshot(runDir)
	if !inRepo {
		fmt.Fprintf(os.Stderr, "[run] note: %s is not a git repository — files_changed will be empty in the Result\n", runDir)
	}

	collector := newRunCollector(opts.Budget)

	store, err := session.NewStore(cfg)
	if err != nil {
		return runOutcome{}, res, nil, sessionName, err
	}

	h, err := spawnDaemon(sessionName, cfg.LogLevel, configPath, false)
	if err != nil {
		return runOutcome{}, res, nil, sessionName, err
	}
	defer h.cleanup()
	rec, err := waitForSession(store, sessionName, runBootTimeout, h)
	if err != nil {
		// The daemon may have half-registered; remove whatever exists.
		cleanupRunSession(store, sessionName)
		return runOutcome{}, res, nil, sessionName, daemonStartError(sessionName, err)
	}
	fmt.Fprintf(os.Stderr, "[run] session %s up (PID %d, NATS %d)\n", sessionName, rec.PID, rec.NATSPort)

	outcome := runHeadlessPrompt(rec, opts, collector)
	finished := time.Now()

	// Assemble the Result and the audit evidence BEFORE teardown (the
	// worktree — diffs included — is removed on return), from what the run
	// actually did.
	var after map[string]string
	if inRepo {
		after, _ = gitStatusSnapshot(runDir)
		res.FilesChanged = changedPaths(before, after)
	}
	res.Commands = collector.CommandLines()
	res.Output = collector.OutputText()
	res.TokensUsed = int(collector.TokensUsed())
	diff := captureAuditDiff(runDir, res.FilesChanged, after, inRepo)
	audit := assembleRunAudit(collector, opts, providerName, sessionName, outcome, res, diff, start, finished)

	if opts.Keep {
		fmt.Fprintf(os.Stderr, progname.Rewrite("[run] --keep: session %s left running; attach with \"rysh attach %s\"\n"),
			sessionName, sessionName)
	} else {
		cleanupRunSession(store, sessionName)
		fmt.Fprintf(os.Stderr, "[run] session %s stopped and deleted\n", sessionName)
	}
	return outcome, res, audit, sessionName, nil
}

// cleanupRunSession stops the throwaway daemon gracefully (so it flushes KV
// and marks its record) then deletes the record, KV buckets, and working-dir
// artifacts. Scoped strictly to the named run-* session.
func cleanupRunSession(store *session.Store, name string) {
	if rec, err := store.Get(name); err == nil && rec.PID > 0 && session.ProcessAlive(rec.PID) {
		_ = session.Terminate(rec.PID, 8*time.Second)
	}
	_ = store.Delete(name)
}

// runHeadlessPrompt drives the already-booted throwaway session: waits for the
// active pane, subscribes to agentic status / output / step / usage / approval
// subjects, submits the prompt in prompt mode, and feeds decideRunOutcome.
// The collector accumulates the observable facts (output text, bash commands,
// token spend) that become the run's eval.Result; when a --budget is set the
// usage stream doubles as the enforcement point.
func runHeadlessPrompt(rec session.Record, opts runOptions, collector *runCollector) runOutcome {
	deadline := time.Now().Add(opts.Timeout)
	msg.SetSessionPrefix(rec.Name)

	nc, err := nats.Connect(fmt.Sprintf("nats://127.0.0.1:%d", rec.NATSPort),
		nats.Timeout(3*time.Second), nats.MaxReconnects(0))
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: fmt.Sprintf("connect to session NATS: %v", err)}
	}
	defer nc.Close()
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	// Wait for the workspace to have an active pane — the daemon creates its
	// initial tab/pane asynchronously after NATS comes up.
	paneID, err := waitForActivePane(pub, deadline)
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: err.Error()}
	}

	// Buffered so slow stdout can never block a NATS callback; pushEvent drops
	// on overflow, which is safe because the consumer stops at the FIRST
	// terminal event — anything queued behind 256 pending events is either
	// non-terminal noise or moot.
	events := make(chan runEvent, 256)
	pushEvent := func(ev runEvent) {
		select {
		case events <- ev:
		default:
		}
	}
	decode := func(m *nats.Msg) interface{} {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return nil
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return nil
		}
		return decoded
	}

	// Agentic phase transitions decide the exit code.
	subStatus, err := nc.Subscribe(msg.T("pane", "*", "llm_prompt_execution", "status"), func(m *nats.Msg) {
		if st, ok := decode(m).(*msg.MsgAgenticStatus); ok {
			pushEvent(runEvent{Kind: runEvPhase, Phase: st.Phase})
		}
	})
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: fmt.Sprintf("subscribe status: %v", err)}
	}
	defer func() { _ = subStatus.Unsubscribe() }()

	// Agent output streams to stdout (text) / stderr (errors) as it arrives,
	// and accumulates into the Result's Output.
	subOutput, err := nc.Subscribe(msg.T("pane", "*", "llm_prompt_execution", "output"), func(m *nats.Msg) {
		out, ok := decode(m).(*msg.MsgAgenticOutput)
		if !ok {
			return
		}
		collector.OnOutput(out)
		switch out.Type {
		case "text":
			fmt.Print(out.Content)
		case "error":
			fmt.Fprintf(os.Stderr, "[agent error] %s\n", strings.TrimSpace(out.Content))
		}
	})
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: fmt.Sprintf("subscribe output: %v", err)}
	}
	defer func() { _ = subOutput.Unsubscribe() }()

	// Structured step events carry the executed bash commands for the Result.
	subSteps, err := nc.Subscribe(msg.T("pane", "*", "llm_prompt_execution", "steps"), func(m *nats.Msg) {
		if st, ok := decode(m).(*msg.MsgAgenticStep); ok {
			collector.OnStep(st)
		}
	})
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: fmt.Sprintf("subscribe steps: %v", err)}
	}
	defer func() { _ = subSteps.Unsubscribe() }()

	// Usage-ledger records (design 003: one schema, many producers) supply the
	// Result's token count and enforce --budget. A provider that emits no
	// usage records simply leaves TokensUsed at 0 — never fabricated.
	subUsage, err := nc.Subscribe(msg.T("pane", "*", "usage"), func(m *nats.Msg) {
		urec, ok := decode(m).(*msg.MsgUsageRecord)
		if !ok {
			return
		}
		if detail, breached := collector.OnUsage(urec); breached {
			fmt.Fprintf(os.Stderr, "[run] %s\n", detail)
			pushEvent(runEvent{Kind: runEvBudget, Detail: detail})
		}
	})
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: fmt.Sprintf("subscribe usage: %v", err)}
	}
	defer func() { _ = subUsage.Unsubscribe() }()

	// Approval gates fail the run closed (gate-blocked): headless CI has no
	// approver. The request is also recorded verbatim for the audit artifact
	// (decision fail_closed; policy rule parsed from the description).
	subApproval, err := nc.Subscribe(msg.T("pane", "*", "approval", "request"), func(m *nats.Msg) {
		if req, ok := decode(m).(*msg.MsgApprovalRequest); ok {
			collector.OnApproval(req)
			detail := fmt.Sprintf("approval requested (%s): %s", req.Type, req.Description)
			fmt.Fprintf(os.Stderr, "[run] %s\n", detail)
			pushEvent(runEvent{Kind: runEvApproval, Detail: detail})
		}
	})
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: fmt.Sprintf("subscribe approvals: %v", err)}
	}
	defer func() { _ = subApproval.Unsubscribe() }()

	// Governance-proxy audit records (design 001 §4.5) carry the only
	// bus-observable SNAT redaction counts. They fire only when traffic
	// routes through the proxy (wrapped third-party CLIs); a run whose agent
	// calls the provider directly legitimately sees none.
	subProxy, err := nc.Subscribe(msg.T("pane", "*", "proxy", "audit"), func(m *nats.Msg) {
		if rec, ok := decode(m).(*msg.MsgProxyRequestAudit); ok {
			collector.OnProxyAudit(rec)
		}
	})
	if err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: fmt.Sprintf("subscribe proxy audit: %v", err)}
	}
	defer func() { _ = subProxy.Unsubscribe() }()

	// Ctrl+C / SIGTERM reads as a cancellation event so cleanup still runs.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		if _, ok := <-sigCh; ok {
			pushEvent(runEvent{Kind: runEvPhase, Phase: "cancelled"})
		}
	}()

	// The pane's LLMPromptExecutionActor subscribes shortly after the pane
	// appears in the snapshot; give the subscription a moment so the prompt
	// (core NATS, no persistence) cannot be published into the void.
	time.Sleep(500 * time.Millisecond)

	if err := pub.Send(msg.T("ws", "inbox"), &msg.MsgSubmitInput{
		Text: opts.Prompt, Mode: "prompt", PaneID: paneID,
	}); err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: fmt.Sprintf("submit prompt: %v", err)}
	}
	if err := pub.Flush(); err != nil {
		return runOutcome{Status: "error", ExitCode: runExitError, Detail: fmt.Sprintf("flush prompt: %v", err)}
	}
	fmt.Fprintf(os.Stderr, "[run] prompt submitted to pane %s\n", paneID)

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	outcome := decideRunOutcome(events, timer.C)
	// Final text/steps/usage can trail the terminal status by one scheduler
	// beat even though the orchestrator flushes output first; drain briefly so
	// the Result carries the complete answer.
	time.Sleep(150 * time.Millisecond)
	return outcome
}

// waitForActivePane polls the workspace snapshot until an active pane exists,
// returning its ID. Bounded by the earlier of the run deadline and the boot
// timeout — a workspace that cannot produce a pane is a boot failure, not
// something to burn the whole --timeout on.
func waitForActivePane(pub *msg.NATSPublisher, deadline time.Time) (string, error) {
	bootDeadline := time.Now().Add(runBootTimeout)
	if bootDeadline.After(deadline) {
		bootDeadline = deadline
	}
	for time.Now().Before(bootDeadline) {
		reply, err := pub.Request(msg.T("ws", "snapshot"), &msg.MsgGetWorkspaceSnapshot{}, 2*time.Second)
		if err == nil {
			if sr, ok := reply.(*msg.MsgWorkspaceSnapshotReply); ok && sr.Snapshot.ActivePaneID != "" {
				return sr.Snapshot.ActivePaneID, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", errors.New("workspace never reported an active pane (daemon booted but produced no pane)")
}
