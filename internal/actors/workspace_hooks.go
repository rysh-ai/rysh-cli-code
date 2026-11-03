// SPDX-License-Identifier: Apache-2.0

package actors

// workspace_hooks.go — the daemon side of lifecycle hooks and automatic
// per-turn checkpoints (gaps G7+G8, out/gap09-design.md).
//
// Three event sources, from most to least exact:
//
//   - a bridged claude pane reports its own events through `rysh hook emit`,
//     which runs the hooks in its own process (internal/hooks/bridge.go) —
//     nothing here is involved beyond writing the --settings file;
//   - a native rysh agent's tool calls pass through the approval policy, where
//     pre-tool hooks are composed in (approvalPolicyFunc);
//   - every agent pane's foreground program start/exit and "needs you" edge
//     (pane_lifecycle.go), which fire on-needs-you / on-finish and take the
//     automatic checkpoints.
//
// Hooks never run on the actor goroutine: a slow hook would freeze the session.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/rysh-ai/rysh-cli-code/internal/checkpoint"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/hooks"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// ---------------------------------------------------------------------------
// loading and ##hooks
// ---------------------------------------------------------------------------

// loadHooks (re)loads .rysh/hooks.yaml and installs everything that depends
// on it. Called at session start and by ##hooks reload.
func (w *WorkspaceActor) loadHooks() {
	prevBridge := claudeBridgeSettings() != ""
	cfg := hooks.Load(w.cfg.RyshDir)
	w.hooks = cfg

	// A file that stopped parsing keeps the bridge it had: switching it off
	// would quietly turn a failing-closed gate into no gate at all.
	bridge := cfg.ClaudeBridge || (cfg.Err != nil && prevBridge)
	path := ""
	if bridge {
		var err error
		if path, err = writeClaudeBridgeSettings(w.cfg.RyshDir, w.sessionName); err != nil {
			slog.Warn("hooks: cannot write the claude bridge settings", "err", err)
			path = ""
		}
	}
	setClaudeBridgeSettings(path)
	setHooksBroken(cfg.Err)
	if cfg.Err != nil {
		slog.Error("hooks: hooks.yaml did not load — pre-tool calls are blocked until it does", "err", cfg.Err)
	}
	setLifecycle(&lifecycle{cfg: cfg, env: w.hookEnv("")})
	w.installApprovalPolicy()
}

// hookEnv is where hooks run (the workspace root) and the rysh variables they
// get on top of the daemon's environment, as user commands get them.
func (w *WorkspaceActor) hookEnv(paneID string) hooks.Env {
	dir := w.baseDir()
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	vars := []string{"RYSH_SESSION=" + w.sessionName}
	if paneID != "" {
		vars = append(vars, "RYSH_PANE="+paneID)
	}
	if self, err := os.Executable(); err == nil {
		vars = append(vars, "RYSH_BIN="+self)
	}
	return hooks.Env{Dir: dir, Vars: vars}
}

const hooksUsage = "usage: ##hooks [list] | reload\n"

// handleHooksCommand implements ##hooks.
func (w *WorkspaceActor) handleHooksCommand(out *strings.Builder, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch sub {
	case "list", "ls", "status":
	case "reload":
		w.loadHooks()
		if w.hooks.Err != nil {
			fmt.Fprintf(out, "hooks: %v\n  pre-tool calls are blocked until the file loads (fix it, then ##hooks reload)\n", w.hooks.Err)
			return w.hooks.Err
		}
		fmt.Fprint(out, "hooks reloaded\n")
	default:
		fmt.Fprint(out, hooksUsage)
		return fmt.Errorf("unknown ##hooks subcommand: %q", sub)
	}
	w.printHooks(out)
	return nil
}

func (w *WorkspaceActor) printHooks(out *strings.Builder) {
	cfg := w.hooks
	if cfg == nil {
		cfg = hooks.Empty()
	}
	switch {
	case cfg.Err != nil:
		fmt.Fprintf(out, "hooks: BROKEN — %v (pre-tool calls are blocked)\n", cfg.Err)
	case !cfg.Loaded:
		fmt.Fprintf(out, "hooks: no %s in %s\n", hooks.FileName, w.cfg.RyshDir)
	default:
		fmt.Fprintf(out, "hooks: %s\n", cfg.Path)
	}
	for _, ev := range hooks.Events {
		for _, h := range cfg.For(ev) {
			line := fmt.Sprintf("  %-13s %s  (timeout %s", ev, h.Run, h.TimeoutOrDefault())
			if h.Match != "" {
				line += ", match " + h.Match
			}
			if ev.Gates() {
				line += ", gates"
			}
			fmt.Fprintln(out, line+")")
		}
	}
	onOff := map[bool]string{true: "on", false: "off"}
	auto := "auto"
	if !cfg.AutoCheckpoints {
		auto = "off"
	}
	fmt.Fprintf(out, "claude bridge: %s · checkpoints: %s\n", onOff[claudeBridgeSettings() != ""], auto)
}

// ---------------------------------------------------------------------------
// native agents: pre-tool through the approval policy
// ---------------------------------------------------------------------------

// preToolHookGate returns the pre-tool check for native rysh agents, or nil
// when no pre-tool hook exists and the file loaded. It runs on the
// orchestrator's goroutine, which is already waiting on this call's verdict.
func (w *WorkspaceActor) preToolHookGate() func(string, []byte) bool {
	cfg := w.hooks
	if cfg == nil || (cfg.Err == nil && len(cfg.For(hooks.PreTool)) == 0) {
		return nil
	}
	env := w.hookEnv("")
	return func(tool string, input []byte) bool {
		ctx, cancel := context.WithTimeout(context.Background(), hooks.MaxTimeout)
		defer cancel()
		blocked, why := cfg.Gate(ctx, hooks.PreTool, hooks.Payload{Program: "rysh", Tool: tool, Input: input}, env)
		if blocked {
			slog.Warn("hooks: pre-tool hook gated a native agent call", "tool", tool, "reason", why)
		}
		return blocked
	}
}

// ---------------------------------------------------------------------------
// the claude bridge's --settings file
// ---------------------------------------------------------------------------

var claudeBridgePath atomic.Value // string

// setClaudeBridgeSettings sets the --settings file every rysh-launched claude
// gets ("" = off). Process-global like the approval policy: nativeAgentCommand
// is a pure function called from several launch paths, and all must agree.
func setClaudeBridgeSettings(path string) { claudeBridgePath.Store(path) }

func claudeBridgeSettings() string {
	s, _ := claudeBridgePath.Load().(string)
	return s
}

// claudeBridgeFile is the generated settings file inside the .rysh dir.
const claudeBridgeFile = "generated/claude-hooks.json"

func writeClaudeBridgeSettings(ryshDir, session string) (string, error) {
	dir, err := filepath.Abs(ryshDir)
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	b, err := hooks.ClaudeSettings(self, dir, session)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, claudeBridgeFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

var hooksBrokenErr atomic.Pointer[error]

// setHooksBroken records whether hooks.yaml failed to load (nil = it loaded,
// or there is none).
func setHooksBroken(err error) {
	if err == nil {
		hooksBrokenErr.Store(nil)
		return
	}
	hooksBrokenErr.Store(&err)
}

// claudeHooksRefusal is why no claude may start now, or "". A hooks.yaml that
// does not load is an unknown posture: claude would start without the bridge,
// so ungated. It is refused instead — sandboxed or not, on every launch path —
// until the file loads (lead ruling R7).
func claudeHooksRefusal(kind string) string {
	p := hooksBrokenErr.Load()
	if p == nil || nativeAgentKind(kind) != nativeAgentClaude {
		return ""
	}
	return fmt.Sprintf("%s does not load (%v) — claude was not started; fix it, then ##hooks reload", hooks.FileName, *p)
}

// claudeRefusalLine is what a pane runs instead of a refused claude: one line
// on stderr saying why.
func claudeRefusalLine(why string) string {
	return "printf '%s\\n' " + shellQuoteAnyShell("rysh: "+why) + " >&2"
}

// claudeBridgeArgs is the flag nativeAgentCommand adds to a claude line.
// Never to a sandboxed claude (the sandbox cannot reach the rysh binary or the
// file; later), and never next to a --settings of the caller's own (a fleet
// pane's): claude takes one, and the caller's is the one it asked for.
func claudeBridgeArgs(s nativeAgentSpec) string {
	path := claudeBridgeSettings()
	if path == "" || nativeAgentKind(s.Kind) != nativeAgentClaude || strings.TrimSpace(s.Sandbox) != "" {
		return ""
	}
	for _, f := range strings.Fields(s.Args) {
		if f == "--settings" || strings.HasPrefix(f, "--settings=") {
			return ""
		}
	}
	return " --settings " + shellQuoteAnyShell(path)
}

// ---------------------------------------------------------------------------
// pane lifecycle: on-needs-you, on-finish, automatic checkpoints
// ---------------------------------------------------------------------------

type paneEventKind int

const (
	paneEventAgentStart paneEventKind = iota
	paneEventNeedsYou
	paneEventFinish
)

// paneEvent is one lifecycle edge of one pane.
type paneEvent struct {
	pane    string
	kind    paneEventKind
	program string
	dir     string
	reason  string
	// sandboxed: the pane's agent runs under a sandbox profile (agent.sandbox).
	sandboxed bool
}

// lifecycle turns pane events into checkpoints and hooks.
type lifecycle struct {
	cfg   *hooks.Config
	env   hooks.Env
	locks sync.Map // pane -> *sync.Mutex: one checkpoint at a time per pane
}

var currentLifecyclePtr atomic.Pointer[lifecycle]

func setLifecycle(l *lifecycle) { currentLifecyclePtr.Store(l) }

func currentLifecycle() *lifecycle { return currentLifecyclePtr.Load() }

// isAgentCLI: the program is a known agent CLI (claude, codex, gemini, …).
func isAgentCLI(program string) bool {
	_, ok := proxy.ProfileFor(program)
	return ok
}

func (l *lifecycle) handle(ctx context.Context, ev paneEvent) {
	agent := isAgentCLI(ev.program)
	// A bridged claude reports needs-you and stop itself, exactly; the screen
	// and process edges would fire the same hooks a second time.
	bridged := ev.program == "claude" && claudeBridgeSettings() != ""
	// A sandboxed agent can write the workspace, where the hook scripts live:
	// the host never runs them on its behalf (lead ruling R3). Its checkpoints
	// still run; they only read the tree, through hardened git.
	runHooks := !bridged && !ev.sandboxed
	env := l.env
	env.Vars = append(append([]string{}, env.Vars...), "RYSH_PANE="+ev.pane)
	payload := hooks.Payload{PaneID: ev.pane, Program: ev.program, Cwd: ev.dir, Reason: ev.reason}

	switch ev.kind {
	case paneEventAgentStart:
		if agent {
			l.checkpoint(ctx, ev, "agent start ("+ev.program+")")
		}
	case paneEventNeedsYou:
		// The agent is asking before it acts: the files are still as the
		// user last saw them, which is the state an undo wants.
		if agent || ev.reason == domain.AttentionReasonApproval {
			l.checkpoint(ctx, ev, "needs you ("+strings.TrimSpace(ev.program+" "+ev.reason)+")")
		}
		if runHooks {
			logHookResults(hooks.NeedsYou, l.cfg.Run(ctx, hooks.NeedsYou, payload, env))
		}
	case paneEventFinish:
		if !agent {
			return
		}
		l.checkpoint(ctx, ev, "agent exit ("+ev.program+")")
		if runHooks {
			logHookResults(hooks.Finish, l.cfg.Run(ctx, hooks.Finish, payload, env))
		}
	}
}

func (l *lifecycle) checkpoint(ctx context.Context, ev paneEvent, label string) {
	if !l.cfg.AutoCheckpoints || ev.dir == "" || ev.pane == "" {
		return
	}
	mu, _ := l.locks.LoadOrStore(ev.pane, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	if !m.TryLock() {
		return // one is running for this pane; it will capture the same state
	}
	defer m.Unlock()
	store, err := checkpoint.Open(ev.dir, ev.pane)
	if err != nil {
		return // not a git work tree: nothing to checkpoint
	}
	if _, _, err := store.Save(ctx, label); err != nil {
		lvl := slog.LevelWarn
		if errors.Is(err, checkpoint.ErrTooLarge) {
			lvl = slog.LevelInfo
		}
		slog.Log(ctx, lvl, "checkpoint: automatic save skipped", "pane", ev.pane, "err", err)
	}
}

func logHookResults(ev hooks.Event, results []hooks.Result) {
	for _, r := range results {
		if r.Err != nil {
			slog.Warn("hooks: hook failed", "event", string(ev), "run", r.Run, "err", r.Err)
		}
	}
}
