package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/rysh-ai/rysh-cli-code/internal/actors"
	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/browserinstance"
	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/cdp"
	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/logging"
	"github.com/rysh-ai/rysh-cli-code/internal/mcp"
	"github.com/rysh-ai/rysh-cli-code/internal/metrics"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/pipeline"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
	"github.com/rysh-ai/rysh-cli-code/internal/tui"
)

// version, commit and date are set by goreleaser at build time via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Extract global flags before subcommand dispatch. These are accepted in any
	// position (before or after the subcommand) and are stripped from the args
	// handed to the command router.
	rawArgs := os.Args[1:]
	rawArgs, configPath := extractStringFlag(rawArgs, "--config")
	rawArgs, shared := extractBoolFlag(rawArgs, "--shared")
	rawArgs, upgrade := extractBoolFlag(rawArgs, "--upgrade")
	args, logLevel := extractLogLevel(rawArgs)

	// Resolve --config to an absolute path and validate it exists. Making it
	// absolute ensures the spawned daemon (which inherits the caller's CWD) loads
	// the very same file.
	if configPath != "" {
		if abs, err := filepath.Abs(configPath); err == nil {
			configPath = abs
		}
		if _, err := os.Stat(configPath); err != nil {
			fmt.Fprintf(os.Stderr, progname.Rewrite("rysh: config file not found: %s\n"), configPath)
			os.Exit(1)
		}
	}

	cfg := config.LoadFrom(configPath)
	cfg.ShareFirstTab = shared

	if logLevel != "" {
		if !logging.ValidLevel(logLevel) {
			fmt.Fprintf(os.Stderr, progname.Rewrite("rysh: invalid log level %q (valid: off, debug, info, warn, error)\n"), logLevel)
			os.Exit(1)
		}
		cfg.LogLevel = logLevel
	}

	logger := logging.Setup(cfg.LogLevel, cfg.SessionName)

	if err := run(cfg, logger, args, configPath, shared, upgrade); err != nil {
		fmt.Fprintf(os.Stderr, progname.Rewrite("rysh: %v\n"), err)
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger, args []string, configPath string, shared, upgrade bool) error {
	// Sweep the registry for orphaned daemons before handling any command, so a
	// dead-PID record or a wedged daemon (alive but no longer serving NATS, e.g.
	// the one that lingered for 8 days holding the NATS port) is cleaned up
	// rather than confusing attach/create/list. Skipped when this process IS the
	// daemon, and for commands that don't touch sessions.
	maybeReapOrphans(cfg, logger, args)

	// Fail fast on a host that cannot allocate a PTY (native Windows). Every
	// pane is a PTY-backed shell, so a session would start, render, and then
	// fail on the first pane with a low-level error. Refusing up front with the
	// supported path is more honest than a broken session.
	if err := requirePTY(args); err != nil {
		return err
	}

	if len(args) == 0 {
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		// If a live session daemon exists, auto-attach to it.
		sessionName := cfg.SessionName
		if sessionName == "" {
			sessionName = "default"
		}
		cfg.SessionName = sessionName
		// Refuse to open a session that belongs to the other front-end (e.g. the
		// CLI opening a session the desktop app created). Checked before both the
		// attach-to-live and spawn-fresh paths.
		if rec, err := store.Get(sessionName); err == nil {
			if serr := session.EnsureSourceMatch(rec, session.NormalizeSource(cfg.SessionSource)); serr != nil {
				return serr
			}
		}
		if rec, err := store.Get(sessionName); err == nil &&
			rec.PID > 0 && session.ProcessAlive(rec.PID) && rec.NATSPort > 0 {
			// Decide whether to restart the daemon (see maybeUpgradeDaemon).
			restart, derr := maybeUpgradeDaemon(cfg, upgrade, rec)
			if derr != nil {
				return derr
			}
			if restart {
				if err := upgradeDaemon(sessionName, rec); err != nil {
					return fmt.Errorf("upgrade: %w", err)
				}
				// fall through to spawn a fresh daemon below
			} else {
				// Clean up stale TUI PIDs before attaching (allows multi-TUI).
				rec.CleanStalePIDs()
				_, _ = store.Upsert(rec)
				return runAttachUI(cfg, logger, store, rec)
			}
		}
		// No live daemon (or stopped for --upgrade) — spawn one and attach.
		h, err := spawnDaemon(sessionName, cfg.LogLevel, configPath, shared)
		if err != nil {
			return err
		}
		defer h.cleanup()
		rec, err := waitForSession(store, sessionName, 10*time.Second, h)
		if err != nil {
			return daemonStartError(sessionName, err)
		}
		return runAttachUI(cfg, logger, store, rec)
	}

	// CLI equivalents of "##" system commands. Any of the rysh-command flags
	// (e.g. --cmd, --pane, --tab, --new, --agent, ...) routes here:
	//   rysh --cmd  --tab-id <t> --pane-id <p> "echo hailo"   == ##cmd echo hailo
	//   rysh --pane --pane-id <p> info                         == ##pane info
	// The flag names the rysh command; --session/--tab-id/--pane-id target the
	// session and pane; the remaining args form the command body.
	if name, ok := detectRyshCmd(args); ok {
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		sess, tabID, paneID, body := parseRyshArgs(args, name)
		if sess == "" {
			sess = cfg.SessionName
		}
		command := name
		if body != "" {
			command = name + " " + body
		}
		return cli.RyshCommand(store, sess, tabID, paneID, command)
	}

	switch args[0] {
	case "attach":
		if len(args) < 2 {
			return errors.New("attach requires a session name")
		}
		cfg.SessionName = args[1]
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		rec, err := store.Get(args[1])
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("session %q not found", args[1])
			}
			return err
		}
		// Refuse to attach to a session that belongs to the other front-end.
		if serr := session.EnsureSourceMatch(rec, session.NormalizeSource(cfg.SessionSource)); serr != nil {
			return serr
		}
		// If the daemon is still alive, attach a TUI client to its NATS server.
		if rec.PID > 0 && session.ProcessAlive(rec.PID) && rec.NATSPort > 0 {
			// Decide whether to restart the daemon: the explicit --upgrade flag
			// forces it; otherwise the configured upgrade-on-attach mode decides
			// based on whether the live daemon's binary differs from this one.
			restart, derr := maybeUpgradeDaemon(cfg, upgrade, rec)
			if derr != nil {
				return derr
			}
			if restart {
				if err := upgradeDaemon(args[1], rec); err != nil {
					return fmt.Errorf("upgrade: %w", err)
				}
				// fall through to spawn a fresh daemon below
			} else {
				// Clean up stale TUI PIDs before attaching (allows multi-TUI).
				rec.CleanStalePIDs()
				_, _ = store.Upsert(rec)
				return runAttachUI(cfg, logger, store, rec)
			}
		}
		// Daemon is dead (or stopped for --upgrade) — spawn a fresh one
		// (will restore from KV) and attach.
		h, serr := spawnDaemon(args[1], cfg.LogLevel, configPath, shared)
		if serr != nil {
			return serr
		}
		defer h.cleanup()
		rec, err = waitForSession(store, args[1], 10*time.Second, h)
		if err != nil {
			return daemonStartError(args[1], err)
		}
		return runAttachUI(cfg, logger, store, rec)
	case "detach":
		if len(args) < 2 {
			return errors.New("detach requires a session name")
		}
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		return detachSession(store, args[1])
	case "list-sessions":
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		return listSessions(store)
	case "install":
		return runRegistryInstall(cfg, args[1:])
	case "search":
		return runRegistrySearch(cfg, args[1:])
	case "update":
		return runRegistryUpdate(cfg, args[1:])
	case "list-packages":
		return runRegistryList(cfg)
	case "eval":
		return runEval(cfg, configPath, args[1:])
	case "run":
		// CI mode (design 009): throwaway detached session, prompt-mode
		// execution, fail-closed exit codes. Exits the process directly so the
		// code reaches the CI runner exactly as decided (0 done, 1 error,
		// 2 partial-saved, 3 gate-blocked, 4 budget-exhausted, 5 timeout);
		// cleanup completes before returning.
		code, rerr := runRunCmd(cfg, configPath, args[1:])
		if rerr != nil {
			fmt.Fprintf(os.Stderr, progname.Rewrite("rysh: %v\n"), rerr)
		}
		os.Exit(code)
		return nil // unreachable; satisfies the compiler after os.Exit
	case "stop":
		if len(args) < 2 {
			return errors.New("stop requires a session name")
		}
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		return stopSession(store, args[1])
	case "delete-session":
		if len(args) < 2 {
			return errors.New("delete-session requires a session name")
		}
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		// Capture the session's workdir BEFORE deleting the record, then reap
		// any headless Chromiums the killed daemon leaves behind (the forced
		// kill skips actor shutdown, so HeadlessBrowserActor never closes its
		// browser).
		var browserRoot string
		if rec, err := store.Get(args[1]); err == nil && rec.Path != "" {
			browserRoot = browserinstance.RootDir(rec.Path)
		}
		if err := store.Delete(args[1]); err != nil {
			return err
		}
		if browserRoot != "" {
			if killed := cdp.SweepOrphanedHeadless(browserRoot); len(killed) > 0 {
				fmt.Printf("reaped %d orphaned headless browser(s)\n", len(killed))
			}
		}
		return nil

	case "send":
		// rysh send <session-name> <input> [--pane <pane-id>] [--mode shell|prompt]
		if len(args) < 3 {
			return errors.New(progname.Rewrite("usage: rysh send <session-name> <input> [--pane <pane-id>] [--mode shell|prompt]"))
		}
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		sessName := args[1]
		inputText := args[2]
		paneID := flagVal(args[3:], "--pane")
		mode := flagVal(args[3:], "--mode")
		return cli.PaneSendInput(store, sessName, paneID, inputText, mode)

	case "web":
		// rysh web start|stop <session> — design 007's top-level entry to the
		// in-daemon web viewer; see web_cmd.go.
		return runWebCmd(cfg, args)

	// --- Forge: API spec → governed agent tools / SDKs / docs / MCP server ---

	case "forge":
		return runForgeCommand(args[1:])

	// --- Entity management CLI commands ---

	case "tab":
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		return runTabCommand(store, args[1:])

	case "lane":
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		return runLaneCommand(store, args[1:])

	case "pane-group":
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		return runPaneGroupCommand(store, args[1:])

	case "pane":
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		return runPaneCommand(store, args[1:])

	case "stacked-pane":
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		return runStackedPaneCommand(store, args[1:])

	case "pipeline":
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		return runPipelineCommand(store, args[1:])

	case "create":
		// rysh create <session-name> [--detached|-d]
		// In detached mode the daemon is spawned and left running headlessly;
		// control returns to the shell prompt instead of attaching a TUI.
		createArgs := args[1:]
		detached := hasFlag(createArgs, "--detached", "-d")
		positional := positionalArgs(createArgs)
		if len(positional) < 1 {
			return errors.New("create requires a session name")
		}
		sessionName := positional[0]
		cfg.SessionName = sessionName
		store, err := session.NewStore(cfg)
		if err != nil {
			return err
		}
		// Refuse to create if a session with that name already exists and is alive.
		if rec, err := store.Get(sessionName); err == nil {
			if rec.PID > 0 && session.ProcessAlive(rec.PID) {
				return fmt.Errorf(progname.Rewrite("session %q is still running (PID %d, state: %s); use \"rysh attach %s\" to reattach or \"rysh stop %s\" to stop it"), sessionName, rec.PID, rec.State, sessionName, sessionName)
			}
			return fmt.Errorf(progname.Rewrite("session %q already exists (state: %s); use \"rysh attach %s\" to reattach or \"rysh delete-session %s\" to remove it"), sessionName, rec.State, sessionName, sessionName)
		}
		// Spawn daemon process.
		h, err := spawnDaemon(sessionName, cfg.LogLevel, configPath, shared)
		if err != nil {
			return err
		}
		defer h.cleanup()
		rec, err := waitForSession(store, sessionName, 10*time.Second, h)
		if err != nil {
			return err
		}
		if detached {
			// Detached mode: leave the daemon running headlessly and return to
			// the shell prompt without attaching a TUI.
			fmt.Printf(progname.Rewrite("[created session %q in detached mode (PID %d); attach with \"rysh attach %s\"]\n"), sessionName, rec.PID, sessionName)
			return nil
		}
		return runAttachUI(cfg, logger, store, rec)

	case "onboard":
		// Guided first-run wizard: provider + key (validated), optional first
		// humanoid, start hand-off. Flag-driven fallback for scripts/no-TTY.
		return runOnboard(cfg, configPath, args[1:], logger)

	case "doctor":
		// Diagnostics: provider / channels / daemon / config checks, each
		// PASS/WARN/FAIL with a one-line fix. Non-zero exit iff any FAIL.
		return runDoctor(cfg, configPath, args[1:])

	case "channel":
		// Channel plugin management: install / list / remove out-of-process
		// channel plugins (openclaw_roadmap design 002, WS2 P1-P3).
		return runChannelCmd(cfg, args[1:])

	case "assistant":
		// Personal assistant mode: one-command single-user always-on humanoid
		// profile with fail-closed safety defaults (design 007, WS7 PM1-PM3).
		return runAssistant(cfg, configPath, args[1:])

	case "daemon":
		// Internal: spawned by spawnDaemon(). Runs the backend (NATS + actors)
		// headlessly; TUI clients attach/detach independently.
		if len(args) < 2 {
			return errors.New("daemon requires a session name")
		}
		cfg.SessionName = args[1]
		return runDaemon(cfg, logger, configPath)

	case "version", "--version", "-V":
		fmt.Printf("rysh %s (commit: %s, built: %s)\n", version, commit, date)
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf(progname.Rewrite("unknown command: %s\nRun \"rysh help\" for usage"), args[0])
	}
}

// runDaemon runs the backend (NATS server + actors + PTYs) as a headless
// daemon process. It is spawned by spawnDaemon() and blocks until SIGTERM/SIGINT.
// TUI clients attach and detach independently via runAttachUI().
func runDaemon(cfg config.Config, logger *slog.Logger, configPath string) error {
	// NOTE: goHeadless() is deferred until after critical init so that errors
	// are visible if the daemon is run directly from a terminal for debugging
	// (e.g. ./rysh daemon test4).

	msg.InitMsgLog(cfg.LogMessages)

	sessionName := cfg.SessionName
	if sessionName == "" {
		sessionName = "default"
	}
	msg.SetSessionPrefix(sessionName)

	// Make the session aware of where its configuration and rysh state live.
	// cfg.ConfigFile / cfg.RyshDir were resolved during config.Load using the
	// search order: <cwd>/rysh.config.yaml, <cwd>/.rysh/rysh.config.yaml,
	// ~/.rysh/rysh.config.yaml. Ensure the rysh dir exists so downstream
	// consumers can rely on it, and surface the decision in the daemon log.
	if cfg.RyshDir != "" {
		if err := os.MkdirAll(cfg.RyshDir, 0o755); err != nil {
			logger.Warn(progname.Rewrite("rysh: could not create rysh dir"), "rysh_dir", cfg.RyshDir, "err", err)
		}
	}
	logger.Info(progname.Rewrite("rysh: resolved config"),
		"config_file", cfg.ConfigFile, "rysh_dir", cfg.RyshDir, "session", sessionName)

	// Reap headless Chromiums orphaned by a previous daemon that died without
	// actor shutdown (force-kill, crash) — mirrors the orphaned-daemon sweep.
	if killed := cdp.SweepOrphanedHeadless(browserinstance.RootDir(daemonWorkDir(cfg))); len(killed) > 0 {
		logger.Info(progname.Rewrite("rysh: reaped orphaned headless browsers"), "pids", killed)
	}

	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}

	// Refuse to (re)open a stopped session that belongs to the other front-end.
	// Backstop for the friendly CLI guard / the app's picker filtering: the
	// daemon owns the session record and the KV state, so this is the last line
	// of defense before they would be reused by the wrong front-end.
	desiredSource := session.NormalizeSource(cfg.SessionSource)
	if existing, gerr := store.Get(sessionName); gerr == nil {
		if serr := session.EnsureSourceMatch(existing, desiredSource); serr != nil {
			return serr
		}
	}

	b, err := bus.New(bus.Config{
		Mode:        cfg.NATS.Mode,
		URL:         cfg.NATS.URL,
		DataDir:     cfg.NATS.DataDir,
		SessionName: cfg.SessionName,
		Port:        cfg.NATS.Port,
	})
	if err != nil {
		return fmt.Errorf("start nats bus: %w", err)
	}
	defer b.Close()

	daemonVer, daemonHash := binaryIdentity()
	// The record Path is the session's project root (the daemon's cwd, where
	// rysh.config.yaml lives). It is reused as the cwd to respawn the daemon
	// (session.SpawnDaemon). The pane working directory is per-workspace and is
	// NOT the session Path.
	record := session.Record{
		Name:       cfg.SessionName,
		Path:       mustGetwd(),
		State:      "detached",
		PID:        os.Getpid(),
		NATSPort:   b.ClientPort(),
		Version:    daemonVer,
		BinHash:    daemonHash,
		ConfigFile: cfg.ConfigFile,
		RyshDir:    cfg.RyshDir,
		Source:     desiredSource,
	}
	if _, err := store.Upsert(record); err != nil {
		return err
	}

	// Critical init done — session record written, waitForSession can find us.
	// Now go headless so the daemon survives terminal close.
	goHeadless()

	// On exit, mark stopped.
	defer func() {
		record.State = "stopped"
		record.PID = 0
		_, _ = store.Upsert(record)
	}()

	// Keep the boot Upsert's guarantee standing for the daemon's whole
	// lifetime: heal the record if anything clobbers it while we live. The
	// guard's stop (registered after the mark-stopped defer above, so it runs
	// first — LIFO) waits out any in-flight check, so shutdown can mark the
	// record stopped without being raced.
	stopRecordGuard := startSessionRecordGuard(store, record, sessionRecordGuardInterval, logger)
	defer stopRecordGuard()

	// Restore pipeline prompts from KV if available.
	if plKV := b.PipelineKV(); plKV != nil {
		_ = pipeline.RestoreFromKV(plKV)
	}

	// Spawn the WorkspaceActor via the proto.actor ActorSystem.
	agSetup := agentic.NewSetup(cfg, b.PipelineKV())
	if agSetup == nil {
		agSetup = agentic.NewSetupAlwaysOn(cfg, b.PipelineKV())
	}
	// Session teardown: cancel every live forge streaming session (gRPC
	// server-streaming / GraphQL subscriptions) so no stream outlives the
	// daemon session that started it. Scoped instances are already torn down
	// by teardownScope; this covers globally-enabled integrations.
	if agSetup.Forge != nil {
		defer agSetup.Forge.Close()
	}

	// Layer in the externalised prompts from rysh-cli-agent-prompts/. This
	// supersedes the in-package fallback consts, assembles the full system
	// prompt (default + project memory + env block + todo guidance), and
	// pushes sub-agent / compaction prompt overrides into rysh-shared.
	{
		workDir, _ := os.Getwd()
		// Reusable reloader closure used by SIGHUP and ##agent reload-prompts
		// (follow-up 2b). Captures the workDir from startup time.
		loadFresh := func() *agentic.Prompts {
			ReloadPromptStore()
			return &agentic.Prompts{
				Default:             LoadPrompt("system_default.md"),
				TodoGuidance:        LoadPrompt("system_todo_guidance.md"),
				EnvBlockTemplate:    LoadPrompt("system_env_block.md"),
				EmailGovernance:     LoadPrompt("system_email_governance.md"),
				SubAgent:            LoadPrompt("system_sub_agent.md"),
				CompactionSummarize: LoadPrompt("system_compaction_summarize.md"),
			}
		}
		agSetup.PromptReloader = loadFresh
		agSetup.ApplyPrompts(loadFresh(), workDir)
	}

	// Wire the in-process metrics sink. Shared across every orchestrator
	// created through this Setup. ##agent metrics dumps it. Follow-up 3b.
	// When the [metrics] flag is on, fan out to a Prometheus sink too and
	// serve it on /metrics; the in-process sink stays primary so ##agent
	// metrics is unaffected.
	{
		inproc := metrics.New()
		if cfg.MetricsEnabled {
			prom := metrics.NewPrometheus()
			agSetup.Metrics = metrics.NewMulti(inproc, prom)
			stopMetrics := startMetricsServer(cfg.MetricsListen, prom.Handler())
			defer func() {
				sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = stopMetrics(sctx)
				scancel()
			}()
		} else {
			agSetup.Metrics = inproc
		}
	}

	// Follow-up 6b: surface live MCP server restart state to the TUI. The
	// manager fires a StatusEvent on every transition; forward it as
	// MsgMCPStatus on the session-global mcp.status subject, which the TUI
	// footer renders. Installed BEFORE Bootstrap so the initial connect wave
	// and the heartbeat both report.
	if agSetup.MCP != nil {
		pub := b.Publisher()
		agSetup.MCP.SetStatusEmitter(func(ev mcp.StatusEvent) {
			_ = pub.Send(msg.T("mcp", "status"), &msg.MsgMCPStatus{
				Server:  ev.Server,
				Phase:   string(ev.Phase),
				Attempt: ev.Attempt,
				Max:     ev.Max,
				Detail:  ev.Detail,
			})
		})
	}

	// Connect to configured MCP servers (.rysh/mcp.json) and register their
	// tools into the shared registry BEFORE the farm spawns any pane — panes
	// clone the registry at creation, so this ordering makes MCP tools visible
	// to every pane and agent. Bounded internally; a down server is skipped.
	{
		mcpCtx, mcpCancel := context.WithTimeout(context.Background(), 45*time.Second)
		agSetup.BootstrapMCP(mcpCtx)
		mcpCancel()
	}

	// Enable Forge integrations (.rysh/forge) marked enabled, registering their
	// generated tools into the shared registry before any pane clones it.
	{
		fCtx, fCancel := context.WithTimeout(context.Background(), 30*time.Second)
		agSetup.BootstrapForge(fCtx)
		fCancel()
	}

	// The WorkspaceFarmActor is the session root. It spawns one WorkspaceActor
	// per workspace (config-defined, falling back to a single workspace) and
	// keeps exactly one active at a time. The session-scoped ws.* subjects are
	// owned by whichever WorkspaceActor is currently active, so the TUI and the
	// rest of the system are unchanged. Subscription limits are enforced
	// per-workspace: each WorkspaceActor builds its own limits checker from its
	// resolved upstream config (its own api_key) and fetches limits in the
	// background.
	farmActor := actors.NewWorkspaceFarmActor(
		cfg,
		b.Publisher(),
		agSetup,
		b.Conn(),
		b.WorkspaceKV(),
		b.PaneKV(),
		b.AgentKV(),
		b.SecretKV(),
		b.VariableKV(),
		sessionName,
		configPath,
	)
	farmProps := actor.PropsFromProducer(func() actor.Actor { return farmActor })
	_, err = b.ActorSystem().Root.SpawnNamed(farmProps, "workspace-farm")
	if err != nil {
		return fmt.Errorf("spawn workspace farm actor: %w", err)
	}

	// Follow-up item 2: SIGHUP triggers prompt hot-reload through the same
	// reloader the ##agent reload-prompts command uses (item 2b). The
	// SIGHUP path here ONLY updates the Setup; broadcasting MsgReloadPrompts
	// to active pane / agent / humanoid actors is owned by the workspace
	// command path (which has the actor topology to enumerate).
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	go func() {
		for range hupCh {
			agSetup.Reload() // re-reads disk, applies to Setup; in-flight actors keep their snapshot
			slog.Info(progname.Rewrite("rysh: prompt store reloaded via SIGHUP"), "files", len(PromptInventory()))
		}
	}()

	// Follow-up 2b: when a prompt override directory is configured, watch it and
	// auto-reload on edit (debounced). The watcher only publishes a trigger to
	// ws.inbox; the active WorkspaceActor runs the same reload + broadcast as
	// ##agent reload-prompts, so a stale-prompt edit takes effect on the next
	// user prompt without a manual command or SIGHUP. No-op when no override dir.
	if dir := PromptOverrideDir(); dir != "" {
		pub := b.Publisher()
		onReload := func() {
			_ = pub.Send(msg.T("ws", "inbox"), &msg.MsgReloadPromptsRequest{Reason: "fsnotify"})
		}
		if stopWatch, werr := startPromptWatcher(dir, onReload); werr != nil {
			slog.Warn(progname.Rewrite("rysh: prompt auto-reload watcher disabled"), "dir", dir, "err", werr)
		} else if stopWatch != nil {
			defer stopWatch()
			slog.Info(progname.Rewrite("rysh: prompt auto-reload watching"), "dir", dir)
		}
	}

	// Block until shutdown signal (SIGTERM from "rysh stop" / "rysh delete-session").
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGTERM, syscall.SIGINT)
	<-shutdownCh

	return nil
}

// runAttachUI connects a TUI-only client to an already-running session daemon.
// The daemon owns the NATS server and actors; this process only drives the TUI.
func runAttachUI(cfg config.Config, logger *slog.Logger, store *session.Store, rec session.Record) error {
	msg.InitMsgLog(cfg.LogMessages)

	// Set the NATS subject prefix to the session name.
	sessionName := cfg.SessionName
	if sessionName == "" {
		sessionName = "default"
	}
	msg.SetSessionPrefix(sessionName)

	// Connect to the session's existing NATS server (external mode).
	b, err := bus.New(bus.Config{
		Mode: "external",
		URL:  fmt.Sprintf("nats://127.0.0.1:%d", rec.NATSPort),
	})
	if err != nil {
		return fmt.Errorf("connect to session %q: %w", rec.Name, err)
	}
	defer b.Close()

	// Register this TUI PID in the session record.
	myPID := os.Getpid()
	rec.AddTUIPID(myPID)
	rec.State = "running"
	_, _ = store.Upsert(rec)

	model, err := tui.NewModel(cfg, logger, b)
	if err != nil {
		return fmt.Errorf(progname.Rewrite("failed to initialize rysh: %w"), err)
	}

	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())

	// SIGUSR1 handler for external detach (Unix only; no-op on Windows).
	stopDetach := setupDetachSignal(program)
	defer stopDetach()

	finalModel, err := program.Run()

	if err != nil {
		return fmt.Errorf("run ui: %w", err)
	}

	// Unregister this TUI PID on exit.
	// Re-read the record to avoid clobbering other TUI PID changes.
	if latest, err := store.Get(rec.Name); err == nil {
		latest.RemoveTUIPID(myPID)
		latest.CleanStalePIDs()
		if len(latest.TUIPIDs) == 0 {
			// Last TUI detached — mark session as detached.
			latest.State = "detached"
		}
		// else: other TUIs still attached, keep state as "running".
		_, _ = store.Upsert(latest)
	}

	if m, ok := finalModel.(tui.Model); ok && m.Detached() {
		fmt.Fprintln(os.Stderr, "[detached from session "+cfg.SessionName+"]")
	}

	return nil
}

// sessionRecordGuardInterval is how often a daemon re-checks that its on-disk
// session record still reflects reality (see startSessionRecordGuard).
const sessionRecordGuardInterval = 30 * time.Second

// startSessionRecordGuard keeps the daemon's session record truthful for the
// daemon's whole lifetime — the standing version of the boot-time Upsert.
// Every interval it re-reads the record and rewrites it when it no longer
// reflects this live daemon: file deleted, PID or NATS port clobbered (e.g.
// by an older rysh build sharing the registry, or a manual edit), or state
// flipped to "stopped" while the daemon is alive. Attach-managed fields
// (TUIPIDs, the running/detached flip) are preserved when the record is
// otherwise ours — see session.SelfHeal. The returned stop function ends the
// guard and waits for any in-flight check, so the shutdown path can mark the
// record stopped without being raced.
func startSessionRecordGuard(store *session.Store, self session.Record, interval time.Duration, logger *slog.Logger) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				existing, err := store.Get(self.Name)
				healed, changed := session.SelfHeal(existing, err == nil, self)
				if !changed {
					continue
				}
				if _, uerr := store.Upsert(healed); uerr != nil {
					logger.Warn(progname.Rewrite("rysh: session record heal failed"), "session", self.Name, "err", uerr)
				} else {
					logger.Info(progname.Rewrite("rysh: healed stale session record"),
						"session", self.Name, "state", healed.State, "pid", healed.PID)
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// daemonHandle tracks a daemon process spawned by spawnDaemon so waitForSession
// can tell "still booting" apart from "already dead". exited closes when the
// child process terminates; errLog is a temp file capturing whatever the child
// wrote to stderr before goHeadless() redirected its stdio to /dev/null.
//
// That capture matters because every startup refusal — a session owned by the
// other front-end, an unparsable config, a NATS port already held — is printed
// on stderr and nowhere else. Without it a daemon that died in 20ms is
// indistinguishable from one that is merely slow, and the caller can only
// report a 10-second timeout.
type daemonHandle struct {
	exited <-chan struct{}
	errLog string
}

// cleanup removes the captured-stderr temp file. Callers defer it right after a
// successful spawnDaemon; it is safe to call on a zero handle.
func (h daemonHandle) cleanup() {
	if h.errLog != "" {
		_ = os.Remove(h.errLog)
	}
}

// startupError returns what the daemon printed on its way out, stripped of the
// "rysh: " prefix main() adds, or "" when it died silently.
func (h daemonHandle) startupError() string {
	if h.errLog == "" {
		return ""
	}
	data, err := os.ReadFile(h.errLog)
	if err != nil {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), progname.Rewrite("rysh: ")))
		if line != "" {
			lines = append(lines, line)
		}
	}
	// The daemon logs to a file, not stderr, so this is normally the single
	// fatal line. Keep the tail in case it managed to print more.
	if len(lines) > daemonStartupErrLines {
		lines = lines[len(lines)-daemonStartupErrLines:]
	}
	return strings.Join(lines, "\n  ")
}

// daemonStartupErrLines caps how much of a dead daemon's stderr is quoted back
// to the user.
const daemonStartupErrLines = 5

// daemonExitError reports that the daemon process exited during startup, with
// the reason it printed. It is distinct from a plain timeout so daemonStartError
// can skip the generic "fix the binary" advice when the daemon already said
// exactly what was wrong.
type daemonExitError struct {
	session string
	detail  string
}

func (e *daemonExitError) Error() string {
	if e.detail == "" {
		return fmt.Sprintf("daemon for session %q exited during startup without printing a reason "+
			"(re-run with --log-level debug for the daemon log)", e.session)
	}
	return fmt.Sprintf("daemon for session %q exited during startup:\n  %s", e.session, e.detail)
}

// spawnDaemon starts a background daemon process for the named session.
// The daemon runs `rysh daemon <sessionName>` in a new process group,
// detached from the calling terminal. The global flags are propagated so the
// daemon process loads the same config and honors --shared: logLevel via
// --log-level, configPath via --config, and shared via --shared.
//
// The returned handle lets waitForSession surface the daemon's own startup
// error; callers should `defer h.cleanup()` to drop the captured stderr file.
func spawnDaemon(sessionName, logLevel, configPath string, shared bool) (daemonHandle, error) {
	exe, err := os.Executable()
	if err != nil {
		return daemonHandle{}, fmt.Errorf("resolve executable path: %w", err)
	}
	args := []string{"daemon", sessionName}
	// Propagate the log level to the daemon only when logging is actually
	// enabled. Logging is off by default, so "", "off" and "none" are skipped.
	if logLevel != "" && logLevel != "off" && logLevel != "none" {
		args = append(args, "--log-level", logLevel)
	}
	// Propagate the explicit config path (already absolute) so the daemon reads
	// the same file regardless of its working directory.
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	// Propagate --shared so the daemon auto-shares the first tab on bootstrap.
	if shared {
		args = append(args, "--shared")
	}
	cmd := exec.Command(exe, args...)
	setDaemonSysProcAttr(cmd)
	cmd.Dir = mustGetwd()
	// Don't inherit terminal stdio — daemon redirects to /dev/null via goHeadless().
	cmd.Stdin = nil
	cmd.Stdout = nil
	// …except stderr, which goes to a temp file until goHeadless() takes over,
	// so a refusal to start is recoverable instead of lost to /dev/null.
	var h daemonHandle
	if f, ferr := os.CreateTemp("", "rysh-daemon-*.err"); ferr == nil {
		h.errLog = f.Name()
		cmd.Stderr = f
		defer f.Close()
	}
	if err := cmd.Start(); err != nil {
		h.cleanup()
		return daemonHandle{}, fmt.Errorf("spawn daemon: %w", err)
	}
	// Reap the child rather than Release()ing it: an unwaited child that exits
	// early becomes a zombie, and kill(pid, 0) succeeds on zombies — so
	// ProcessAlive could not tell a booting daemon from a dead one. The daemon
	// is setsid'd with its own stdio, so it still outlives this process.
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	h.exited = exited
	return h, nil
}

// waitForSession polls the session store until the daemon has written its
// NATSPort (meaning it's ready to accept connections), or until timeout. If the
// spawned daemon exits first it returns a *daemonExitError carrying the reason
// the daemon printed, instead of blocking for the full timeout.
func waitForSession(store *session.Store, name string, timeout time.Duration, h daemonHandle) (session.Record, error) {
	deadline := time.Now().Add(timeout)
	for {
		rec, err := store.Get(name)
		if err == nil && rec.NATSPort > 0 && rec.PID > 0 && session.ProcessAlive(rec.PID) {
			return rec, nil
		}
		// Checked after the registry so a daemon that registered and then died
		// is still reported by its own error rather than as a bare timeout.
		select {
		case <-h.exited:
			return session.Record{}, &daemonExitError{session: name, detail: h.startupError()}
		default:
		}
		if time.Now().After(deadline) {
			return session.Record{}, fmt.Errorf("timed out waiting for session %q daemon to start", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// maybeReapOrphans runs the orphaned-daemon sweep (see Store.Reap) unless this
// invocation is the daemon itself or a command that doesn't touch the registry.
// It is best-effort: store/probe failures are logged, never fatal, so a reap
// problem can't block the command the user actually ran.
func maybeReapOrphans(cfg config.Config, logger *slog.Logger, args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "daemon", "version", "--version", "-V", "help", "-h", "--help":
			return
		}
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return // the command path will surface any real store error
	}
	actions, err := store.Reap(session.ReapOptions{})
	if err != nil {
		logger.Debug("session reap failed", "err", err)
		return
	}
	for _, a := range actions {
		if a.Killed {
			logger.Info("reaped orphaned session daemon", "session", a.Name, "reason", a.Reason)
		} else {
			logger.Debug("session reap could not terminate daemon", "session", a.Name, "reason", a.Reason)
		}
	}
}

// stopSession gracefully stops the session's daemon: SIGTERM first (letting it
// flush KV and mark its record stopped), escalating to SIGKILL if it does not
// exit, so a wedged daemon never lingers as an orphan. It blocks until the
// daemon has actually exited.
func stopSession(store *session.Store, name string) error {
	rec, err := store.Get(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session %q not found", name)
		}
		return err
	}
	if rec.PID <= 0 || !session.ProcessAlive(rec.PID) {
		rec.State = "stopped"
		rec.PID = 0
		_, _ = store.Upsert(rec)
		fmt.Printf("[session %q is already stopped]\n", name)
		return nil
	}
	fmt.Printf("[stopping session %q (PID %d)]\n", name, rec.PID)
	if err := session.Terminate(rec.PID, 8*time.Second); err != nil {
		return fmt.Errorf("stop session %q (PID %d): %w", name, rec.PID, err)
	}
	// The daemon marks its own record stopped on a graceful (SIGTERM) shutdown,
	// but if it had to be SIGKILLed it never ran that handler — so record the
	// stopped state here too, keeping the registry self-consistent either way.
	rec.State = "stopped"
	rec.PID = 0
	rec.TUIPIDs = nil
	_, _ = store.Upsert(rec)
	fmt.Printf("[session %q stopped]\n", name)
	return nil
}

// ---------------------------------------------------------------------------
// Daemon version awareness & upgrade-on-attach
// ---------------------------------------------------------------------------

var (
	binIDOnce sync.Once
	binVer    string
	binHash   string
)

// binaryIdentity returns the identity of the currently-running rysh executable:
// its build version string (main.version) and a short content hash of the
// executable file. The hash is what makes upgrade detection work during active
// development: two "-dirty" dev builds at the same commit share a version string
// but differ in content, so the hash changes on every rebuild. Computed once and
// cached. Either value may be "" if it cannot be determined.
func binaryIdentity() (versionStr, hash string) {
	binIDOnce.Do(func() {
		binVer = version
		binHash = hashExecutable()
	})
	return binVer, binHash
}

// hashExecutable returns a short (12 hex char) sha256 of the running executable's
// file contents, or "" if it cannot be read.
func hashExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// daemonOutdated reports whether the live daemon described by rec was launched
// from a different binary than the one now attaching. The content hash is
// authoritative when both sides have one; otherwise it falls back to the version
// string. A daemon with no recorded identity predates version tracking and is
// treated as outdated.
func daemonOutdated(rec session.Record, curVer, curHash string) bool {
	if rec.BinHash == "" && rec.Version == "" {
		return true // started by a pre-version-tracking daemon
	}
	if rec.BinHash != "" && curHash != "" {
		return rec.BinHash != curHash
	}
	return rec.Version != curVer
}

// describeBuild renders a binary identity for display, e.g. "v0.1.18 (ab12cd34ef01)".
func describeBuild(versionStr, hash string) string {
	switch {
	case versionStr != "" && hash != "":
		return fmt.Sprintf("%s (%s)", versionStr, hash)
	case hash != "":
		return hash
	case versionStr != "":
		return versionStr
	default:
		return "unknown build"
	}
}

// maybeUpgradeDaemon decides whether a live daemon should be restarted before
// attaching. forceUpgrade is the explicit --upgrade flag (always restarts).
// Otherwise the decision follows cfg.UpgradeOnAttach (off|warn|prompt|auto) and
// is only relevant when the daemon's binary differs from the current one. It
// prints any user-facing notice itself and returns whether to restart.
func maybeUpgradeDaemon(cfg config.Config, forceUpgrade bool, rec session.Record) (restart bool, err error) {
	if forceUpgrade {
		return true, nil
	}
	curVer, curHash := binaryIdentity()
	if !daemonOutdated(rec, curVer, curHash) {
		return false, nil
	}
	oldDesc := describeBuild(rec.Version, rec.BinHash)
	newDesc := describeBuild(curVer, curHash)
	mode := cfg.UpgradeOnAttach
	if mode == "" {
		mode = "warn"
	}
	switch mode {
	case "off":
		return false, nil
	case "auto":
		fmt.Printf("[session %q daemon is an older build %s — upgrading to %s]\n", rec.Name, oldDesc, newDesc)
		return true, nil
	case "prompt":
		fmt.Printf("session %q daemon is running an older build %s; current binary is %s.\n", rec.Name, oldDesc, newDesc)
		return promptYesNo("restart the daemon to apply the new version? [y/N] "), nil
	default: // "warn"
		fmt.Printf("[note: session %q daemon is an older build %s; current binary is %s — re-run with --upgrade to apply it]\n", rec.Name, oldDesc, newDesc)
		return false, nil
	}
}

// promptYesNo reads a single line from stdin and reports whether it is an
// affirmative answer (y/yes). Defaults to no on EOF or any read error, so a
// non-interactive invocation never blocks indefinitely on a missing answer.
func promptYesNo(prompt string) bool {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// upgradeDaemon stops a live daemon so a fresh one (built from the current
// binary) can replace it. It first warns about any other attached TUIs, which
// will lose their connection when the daemon exits (they do not auto-reconnect),
// then stops the daemon and waits for it to flush state to KV and exit.
func upgradeDaemon(sessionName string, rec session.Record) error {
	if others := rec.AliveTUIPIDs(); len(others) > 0 {
		// Exclude this process if it somehow appears (it shouldn't pre-attach).
		me := os.Getpid()
		count := 0
		for _, pid := range others {
			if pid != me {
				count++
			}
		}
		if count > 0 {
			fmt.Printf("[warning: %d other TUI(s) attached to session %q will be disconnected by the upgrade and must reattach]\n", count, sessionName)
		}
	}
	fmt.Printf("[upgrading session %q: stopping daemon (PID %d)...]\n", sessionName, rec.PID)
	return stopDaemonAndWait(rec, 10*time.Second)
}

// daemonStartError wraps a waitForSession failure with guidance. State is safe in
// KV (the previous daemon flushed on shutdown), so a failed start — typically a
// broken new build — loses no persisted session state.
func daemonStartError(sessionName string, err error) error {
	// When the daemon printed why it refused to start, that reason IS the
	// message — appending "fix the binary and retry" would only point the user
	// away from the actual cause (e.g. a session owned by the desktop app).
	var exited *daemonExitError
	if errors.As(err, &exited) {
		return err
	}
	return fmt.Errorf("%w\n  the daemon failed to start — your session state is preserved in KV; "+
		progname.Rewrite("fix the binary and run \"rysh attach %s\" again (use --log-level debug to see daemon logs)"), err, sessionName)
}

// ownableSessionName returns a session name the given front-end is allowed to
// open, plus a note when it had to move off the requested one.
//
// The CLI and the desktop app refuse to drive each other's sessions
// (session.EnsureSourceMatch) because they would otherwise share saved layout
// and KV state. Both first-run commands — `rysh onboard` and `rysh assistant` —
// default to the session name "default", which is exactly the name the desktop
// app claims first. A record left behind by the app (even a stopped one, since
// its KV state is still the app's) therefore made the whole first run
// unrecoverable: the spawned daemon hit the guard and exited, and the caller saw
// only "timed out waiting for session ... daemon to start".
//
// Onboarding must not dead-end on that, so fall back to "<name>-cli"
// ("<name>-cli-2", …) — a name this front-end can own — and say so, rather than
// spawning a daemon that is guaranteed to refuse.
func ownableSessionName(store *session.Store, name, source string) (string, string) {
	if store == nil {
		return name, ""
	}
	rec, err := store.Get(name)
	if err != nil || session.EnsureSourceMatch(rec, source) == nil {
		return name, ""
	}
	owner := session.FrontendName(rec.Source)
	base := name + "-" + session.NormalizeSource(source)
	for i := 1; i <= ownableSessionNameTries; i++ {
		candidate := base
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", base, i)
		}
		next, gerr := store.Get(candidate)
		if gerr != nil || session.EnsureSourceMatch(next, source) == nil {
			return candidate, fmt.Sprintf("session %q belongs to the %s and cannot be opened here; using %q instead", name, owner, candidate)
		}
	}
	// Every candidate is spoken for — keep the requested name so the caller
	// fails with the ownership error rather than looping.
	return name, ""
}

// ownableSessionNameTries bounds the "-cli", "-cli-2", … search in
// ownableSessionName.
const ownableSessionNameTries = 20

// stopDaemonAndWait stops a live session daemon and blocks until it has fully
// exited. It is used by --upgrade so that a fresh daemon built from the current
// binary can take the old one's place without a NATS port / data-dir collision.
// SIGTERM is sent first (within timeout) so the daemon flushes its actor state
// to JetStream KV on shutdown — the replacement daemon restores tabs, panes,
// output buffers and history from KV — and if it does not exit in time it is
// SIGKILLed so the upgrade cannot be blocked by a wedged daemon holding the port.
// Note: live PTY child processes (shells, vim, in-flight builds) do not survive
// the restart — only KV-persisted state is restored.
func stopDaemonAndWait(rec session.Record, timeout time.Duration) error {
	if rec.PID <= 0 || !session.ProcessAlive(rec.PID) {
		return nil // already dead — nothing to stop
	}
	if err := session.Terminate(rec.PID, timeout); err != nil {
		return fmt.Errorf("stop daemon (PID %d): %w", rec.PID, err)
	}
	return nil
}

// detachSession sends SIGUSR1 to all TUI processes attached to the session,
// causing them to detach gracefully (same as pressing ctrl+o d inside the TUI).
func detachSession(store *session.Store, name string) error {
	rec, err := store.Get(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session %q not found", name)
		}
		return err
	}
	if rec.State != "running" {
		return fmt.Errorf("session %q is not running (state: %s)", name, rec.State)
	}

	// Clean stale PIDs first.
	rec.CleanStalePIDs()

	if len(rec.TUIPIDs) == 0 {
		// No live TUIs, just mark as detached.
		rec.State = "detached"
		_, _ = store.Upsert(rec)
		fmt.Printf("[session %q has no attached TUI processes; marked as detached]\n", name)
		return nil
	}

	// Send SIGUSR1 to ALL attached TUI processes.
	var detachErrors []string
	for _, pid := range rec.TUIPIDs {
		if err := sendDetachSignal(pid); err != nil {
			detachErrors = append(detachErrors, fmt.Sprintf("PID %d: %v", pid, err))
		} else {
			fmt.Printf("[detaching session %q TUI (PID %d)]\n", name, pid)
		}
	}

	if len(detachErrors) > 0 {
		return fmt.Errorf("some TUIs failed to detach: %s", strings.Join(detachErrors, "; "))
	}
	return nil
}

func listSessions(store *session.Store) error {
	records, err := store.List()
	if err != nil {
		return err
	}
	curVer, curHash := binaryIdentity()
	for _, record := range records {
		record.CleanStalePIDs()
		// Show the daemon's build for live sessions, flagging it when it differs
		// from the binary running this command (i.e. "rysh attach --upgrade" would
		// restart it).
		build := ""
		if record.PID > 0 && session.ProcessAlive(record.PID) {
			build = "\t" + describeBuild(record.Version, record.BinHash)
			if daemonOutdated(record, curVer, curHash) {
				build += " [outdated]"
			}
		}
		src := session.NormalizeSource(record.Source) // blank legacy records show as "cli"
		// The State field tracks TUI attachment only, so a healthy app daemon
		// reads "detached" even while the desktop app is connected. Render the
		// live app connection (Record.AppClients, maintained by the daemon's
		// web hub) as an attachment instead.
		state := record.State
		if src == "app" && record.AppClients > 0 &&
			record.PID > 0 && session.ProcessAlive(record.PID) {
			state = "attached (app)"
		}
		tuiCount := len(record.TUIPIDs)
		if tuiCount > 0 {
			fmt.Printf("%s\t%s\t%s\t%d TUI(s)\t%s%s\n", record.Name, state, src, tuiCount, record.Path, build)
		} else {
			fmt.Printf("%s\t%s\t%s\t%s%s\n", record.Name, state, src, record.Path, build)
		}
	}
	return nil
}

func printUsage() {
	usageLine("usage: rysh [--config <path>] [--shared] [--log-level off|debug|info|warn|error]")
	usageLine("       rysh create <session-name> [--detached|-d]")
	usageLine("       rysh attach <session-name> [--upgrade]")
	usageLine("       rysh detach <session-name>")
	usageLine("       rysh stop <session-name>")
	usageLine("       rysh list-sessions")
	usageLine("       rysh delete-session <session-name>")
	usageLine("       rysh send <session-name> <input> [--pane <pane-id>] [--mode shell|prompt]")
	usageLine("       rysh run [--timeout <dur>] [--json] [--keep] \"<prompt>\"")
	usageLine("                    CI mode: boot a throwaway detached session, run the prompt")
	usageLine("                    agentically, stream output, then stop+delete the session.")
	usageLine("                    Fail-closed exits: 0 done, 1 error, 2 approval requested")
	usageLine("                    (no human to approve), 3 timeout (default 10m). --keep")
	usageLine("                    leaves the session running; --json prints a final result line.")
	usageLine("       rysh web start <session-name> [--control] [--port <n>] [--host <h>] [--no-token]")
	usageLine("       rysh web stop <session-name>")
	usageLine("                    start/stop the session's web viewer and print the full")
	usageLine("                    ?token= URL (the token is generated client-side, so it")
	usageLine("                    works for detached sessions whose pane output is unseen).")
	usageLine("       rysh install <@ns/name[@version]|dir|tarball|url> [--yes] [--force]")
	usageLine("       rysh search [query]        find packages in the registry index")
	usageLine("       rysh update [@ns/name]     reinstall packages the index has newer versions of")
	usageLine("       rysh list-packages         show installed packages (.rysh/rysh.lock)")
	usageLine("       rysh onboard [--provider <name> --key-env <VAR> [--model <m>] [--no-validate]]")
	usageLine("                    guided first-run setup for using rysh AT the keyboard:")
	usageLine("                    provider + key (validated) and terminal prefs written to")
	usageLine("                    rysh.config.yaml, then opens your session (--no-launch skips).")
	usageLine("                    Interactive in a TTY; flag-driven otherwise. Project-local.")
	usageLine("       rysh assistant [--channel <name>] [--owner <id>] [--install-daemon]")
	usageLine("                    guided setup for driving this session AWAY from the keyboard:")
	usageLine("                    pairs Slack/Telegram/Discord/WhatsApp/Signal/iMessage to an")
	usageLine("                    assistant that can operate the session. Fail-closed by default.")
	usageLine("       rysh doctor  diagnostics: provider, channels, daemon/NATS, config checks,")
	usageLine("                    each PASS/WARN/FAIL with a one-line fix.")
	fmt.Println()
	usageLine("global flags:")
	usageLine("  --config <path>      Load configuration from <path> instead of searching")
	usageLine("                       ./rysh.config.yaml then ./.rysh/rysh.config.yaml.")
	usageLine("                       There is no global location: config and state are")
	usageLine("                       project-local, relative to the working directory.")
	usageLine("  --shared             Auto-share the first tab to the upstream workspace")
	usageLine("                       once the initial tabs/panes are created. Requires an")
	usageLine("                       enabled [upstream] block in the config.")
	usageLine("  --upgrade            On attach, stop a session's running daemon and start a")
	usageLine("                       fresh one from the current binary (restoring state from")
	usageLine("                       KV). Use after rebuilding rysh so the new version takes")
	usageLine("                       effect. Live PTY processes (shells, vim, builds) are lost.")
	usageLine("                       Without this flag, attach auto-detects a binary mismatch")
	usageLine("                       and follows [session] upgrade_on_attach (RYSH_UPGRADE_ON_ATTACH):")
	usageLine("                       off|warn (default)|prompt|auto. \"rysh list-sessions\" flags")
	usageLine("                       a live daemon built from an older binary as [outdated].")
	usageLine("  --log-level <level>  Set log level: off, debug, info, warn, error (default: off)")
	usageLine("                       Logging is disabled by default; pass a level here or set")
	usageLine("                       log_level in rysh.config.yaml to enable it.")
	usageLine("                       When debug, logs are saved to /tmp/rysh/logs/")
	fmt.Println()
	usageLine("entity management (requires a running session):")
	usageLine("       rysh tab <session-name> list")
	usageLine("       rysh tab <session-name> create")
	usageLine("       rysh tab <session-name> delete <tab-id>")
	fmt.Println()
	usageLine("       rysh lane <session-name> <tab-id> list")
	usageLine("       rysh lane <session-name> <tab-id> create")
	usageLine("       rysh lane <session-name> <tab-id> delete <lane-id>")
	fmt.Println()
	usageLine("       rysh pane <session-name> <tab-id> <lane-id> list")
	usageLine("       rysh pane <session-name> <tab-id> <lane-id> create [--pane-group <group-id>]")
	usageLine("       rysh pane <session-name> <tab-id> <lane-id> delete <pane-id>")
	fmt.Println()
	usageLine("       rysh pane-group list [--session <name>] [--tab <tab-id>] [--lane <lane-id>]")
	usageLine("       rysh pane-group create [--session <name>] [--tab <tab-id>] [--lane <lane-id>]")
	usageLine("       rysh pane-group delete --id <group-id> [--session <name>] [--tab <tab-id>] [--lane <lane-id>]")
	fmt.Println()
	usageLine("       rysh stacked-pane list [--session <name>] [--pane-group <group-id>]")
	usageLine("       rysh stacked-pane create [--session <name>] [--tab <tab-id>] [--lane <lane-id>] [--pane-group <group-id>]")
	fmt.Println()
	usageLine("       rysh pipeline list [--session <name>]")
	usageLine("       rysh pipeline enable [--session <name>] [--tab <tab-id>]")
	usageLine("       rysh pipeline disable [--session <name>] [--tab <tab-id>]")
	fmt.Println()
	usageLine("## system commands (CLI equivalents, requires a running session):")
	usageLine("       rysh --<command> [--session <name>] [--tab-id <id>] [--pane-id <id>] [args...]")
	usageLine("       runs \"##<command> args...\" on the target pane and prints its output.")
	usageLine("       targeting: --pane-id wins; else --tab-id's active pane; else active pane.")
	fmt.Println()
	usageLine("       rysh --tab        list | list-panes [--tab <id>] | name <name> | delete <id>")
	usageLine("                         | pipeline enable|disable")
	usageLine("       rysh --pane       info | list [--tab <id>] | name <name> | listen <id> | unlisten")
	usageLine("                         | delete <id> | share start|stop|status | approval-pane <names>|clear|list")
	usageLine("       rysh --lane       list | info | name <name> | delete <id>")
	usageLine("       rysh --pg|--panegroup  info | list | layout | delete <id>")
	usageLine("       rysh --new        tab | lane [tab] | pane [tab] [lane] | grid <RxC>")
	usageLine("       rysh --cmd        <pane|pg|lane|tab|ws> [selectors] <bash command>")
	usageLine("       rysh --public     pane print")
	usageLine("       rysh --private    pane print")
	usageLine("       rysh --snap       [private|public]")
	usageLine("       rysh --history")
	usageLine("       rysh --hop        <pane-name> | resume | status | clear")
	usageLine("       rysh --share      pane|panegroup|lane|tab [view|control] | list | status")
	usageLine("       rysh --unshare    pane|panegroup|lane|tab")
	usageLine("       rysh --upstream   status | my-shares | list-remote")
	usageLine("                         | subscribe <shareID> [view|control] | unsubscribe [shareID] | send <text>")
	usageLine("       rysh --ws|--workspace  list | create <name> <api_key>")
	usageLine("       rysh --pipe|--pipeline  help|list|load|unload|show|build|run|status|clear|name|placeholder")
	usageLine("       rysh --auto       web|task|agent|humanoid|code  run|resume|continue|list|show|save|delete|results")
	usageLine("       rysh --agent      spawn | spawn-all <dir> | list | delete <name> | activate | deactivate")
	usageLine("                         | register-output | unregister-output")
	usageLine("       rysh --humanoid   spawn | spawn-all <dir> | stop <name> | list [all|instances|artefacts]")
	usageLine("                         | activate | deactivate")
	usageLine("                         | channels | channel start|stop | register-output | unregister-output")
	usageLine("       rysh --rysh       new ... | tab name <n> | lane name <n> | web start|stop|status")
	fmt.Println()
	usageLine("       examples:")
	usageLine("         rysh --cmd --tab-id <t> --pane-id <p> \"echo hailo\"  (== ##cmd echo hailo)")
	usageLine("         rysh --pane --pane-id <p> info                       (== ##pane info)")
	usageLine("         rysh --tab list                                      (== ##tab list)")
	usageLine("         rysh --share --pane-id <p> pane view                 (== ##share pane view)")
	usageLine("         rysh --unshare --pane-id <p> pane                    (== ##unshare pane)")
	usageLine("         rysh --upstream subscribe <shareID> control          (== ##upstream subscribe ...)")
}

// ---------------------------------------------------------------------------
// CLI command routing helpers
// ---------------------------------------------------------------------------

// extractLogLevel pulls --log-level <value> from args and returns the remaining
// args plus the log level value. If --log-level is not found, level is "".
func extractLogLevel(args []string) (remaining []string, level string) {
	return extractStringFlag(args, "--log-level")
}

// extractStringFlag pulls "<name> <value>" from args and returns the remaining
// args plus the value. If name is not found (or has no following value), value
// is "" and args are returned unchanged.
func extractStringFlag(args []string, name string) (remaining []string, value string) {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			value = args[i+1]
			remaining = append(remaining, args[:i]...)
			remaining = append(remaining, args[i+2:]...)
			return remaining, value
		}
	}
	return args, ""
}

// extractBoolFlag removes any occurrence of the given boolean flag names from
// args, returning the remaining args and whether the flag was present.
func extractBoolFlag(args []string, names ...string) (remaining []string, found bool) {
	for _, a := range args {
		isFlag := false
		for _, n := range names {
			if a == n {
				isFlag = true
				found = true
				break
			}
		}
		if !isFlag {
			remaining = append(remaining, a)
		}
	}
	return remaining, found
}

// flagVal extracts the value of a named flag from args (e.g. --session foo).
func flagVal(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// hasFlag reports whether any of the given flag names appears in args
// (e.g. hasFlag(args, "--detached", "-d")).
func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

// positionalArgs returns the args that are not flags (i.e. do not start with "-").
func positionalArgs(args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// ---------------------------------------------------------------------------
// Rysh "##" command flags
// ---------------------------------------------------------------------------

// ryshCmdNames is the set of top-level "##" system commands that have a
// command-line equivalent flag. Each maps "rysh --<name> ..." to "##<name> ...".
// (--help is intentionally excluded so it keeps showing the rysh usage text.)
var ryshCmdNames = map[string]bool{
	"tab":       true,
	"pane":      true,
	"lane":      true,
	"panegroup": true,
	"pg":        true,
	"public":    true,
	"private":   true,
	"history":   true,
	"pipe":      true,
	"pipeline":  true,
	"share":     true,
	"unshare":   true,
	"upstream":  true,
	"snap":      true,
	"workspace": true,
	"ws":        true,
	"new":       true,
	"cmd":       true,
	"rysh":      true,
	"hop":       true,
	"grounding": true,
	"cron":      true,
	"web":       true,
	"auto":      true,
	"agent":     true,
	"humanoid":  true,
	"snat":      true,
	"rst":       true,
}

// detectRyshCmd scans args for the first rysh-command flag (e.g. --cmd, --pane)
// and returns its bare name (without the leading "--") and whether one was
// found.
// detectRyshCmd reports whether the invocation selects a rysh "##" command
// (e.g. --pane, --cmd, --tab). Only the FIRST argument is examined: a rysh-command
// invocation always leads with its --<command> selector (after global flags are
// stripped), e.g. `rysh --pane --pane-id <p> info`. The bare-word subcommands
// (send, attach, stop, …) and their own options must not be mistaken for it.
//
// Previously this scanned ALL args, so `rysh send <sess> <input> --pane <id>` was
// hijacked as the `--pane` (##pane) command: parseRyshArgs found no --session, the
// session defaulted to cfg.SessionName, and the send failed with
// "session \"default\" not found". Restricting to args[0] keeps the rysh-command
// forms working while letting subcommands own their --pane/--mode flags.
func detectRyshCmd(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	a := args[0]
	if !strings.HasPrefix(a, "--") {
		return "", false
	}
	name := strings.TrimPrefix(a, "--")
	if ryshCmdNames[name] {
		return name, true
	}
	return "", false
}

// parseRyshArgs extracts the session/tab/pane targeting flags and the command
// body for a rysh-command invocation. The command selector flag (--<name>) and
// the targeting flags (--session/--tab-id/--pane-id, first occurrence each) may
// appear anywhere; everything else, in order, forms the command body. Targeting
// flags can therefore precede or follow the body, e.g. both of these work:
//
//	rysh --cmd --tab-id t --pane-id p "echo hailo"
//	rysh --tab list --session mysession
func parseRyshArgs(args []string, name string) (sess, tabID, paneID, body string) {
	cmdFlag := "--" + name
	var parts []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == cmdFlag:
			// command selector flag; not part of the body
		case a == "--session" && i+1 < len(args) && sess == "":
			sess = args[i+1]
			i++
		case a == "--tab-id" && i+1 < len(args) && tabID == "":
			tabID = args[i+1]
			i++
		case a == "--pane-id" && i+1 < len(args) && paneID == "":
			paneID = args[i+1]
			i++
		default:
			parts = append(parts, a)
		}
	}
	body = strings.Join(parts, " ")
	return sess, tabID, paneID, body
}

func runTabCommand(store *session.Store, args []string) error {
	// args = [<session-name>, <subcommand>, ...]
	if len(args) < 2 {
		return errors.New(progname.Rewrite("usage: rysh tab <session-name> <list|create|delete> [<tab-id>]"))
	}
	sess := args[0]
	switch args[1] {
	case "list":
		return cli.TabList(store, sess)
	case "create":
		return cli.TabCreate(store, sess)
	case "delete":
		if len(args) < 3 {
			return errors.New(progname.Rewrite("usage: rysh tab <session-name> delete <tab-id>"))
		}
		return cli.TabDelete(store, sess, args[2])
	default:
		return fmt.Errorf("unknown tab subcommand: %s", args[1])
	}
}

func runLaneCommand(store *session.Store, args []string) error {
	// args = [<session-name>, <tab-id>, <subcommand>, ...]
	if len(args) < 3 {
		return errors.New(progname.Rewrite("usage: rysh lane <session-name> <tab-id> <list|create|delete> [<lane-id>]"))
	}
	sess := args[0]
	tabID := args[1]
	switch args[2] {
	case "list":
		return cli.LaneList(store, sess, tabID)
	case "create":
		return cli.LaneCreate(store, sess, tabID)
	case "delete":
		if len(args) < 4 {
			return errors.New(progname.Rewrite("usage: rysh lane <session-name> <tab-id> delete <lane-id>"))
		}
		return cli.LaneDelete(store, sess, tabID, args[3])
	default:
		return fmt.Errorf("unknown lane subcommand: %s", args[2])
	}
}

func runPaneGroupCommand(store *session.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("pane-group requires a subcommand: list, create, delete")
	}
	sess := flagVal(args, "--session")
	tabID := flagVal(args, "--tab")
	laneID := flagVal(args, "--lane")
	switch args[0] {
	case "list":
		return cli.PaneGroupList(store, sess, tabID, laneID)
	case "create":
		return cli.PaneGroupCreate(store, sess, tabID, laneID)
	case "delete":
		id := flagVal(args, "--id")
		if id == "" {
			return errors.New("pane-group delete requires --id <group-id>")
		}
		return cli.PaneGroupDelete(store, sess, tabID, laneID, id)
	default:
		return fmt.Errorf("unknown pane-group subcommand: %s", args[0])
	}
}

func runPaneCommand(store *session.Store, args []string) error {
	// args = [<session-name>, <tab-id>, <lane-id>, <subcommand>, ...]
	if len(args) < 4 {
		return errors.New(progname.Rewrite("usage: rysh pane <session-name> <tab-id> <lane-id> <list|create|delete> [...]"))
	}
	sess := args[0]
	tabID := args[1]
	laneID := args[2]
	subArgs := args[3:] // subcommand + remaining args
	switch subArgs[0] {
	case "list":
		return cli.PaneList(store, sess, tabID, laneID)
	case "create":
		paneGroupID := flagVal(subArgs, "--pane-group")
		return cli.PaneCreate(store, sess, tabID, laneID, paneGroupID)
	case "delete":
		if len(subArgs) < 2 {
			return errors.New(progname.Rewrite("usage: rysh pane <session-name> <tab-id> <lane-id> delete <pane-id>"))
		}
		return cli.PaneDelete(store, sess, subArgs[1])
	default:
		return fmt.Errorf("unknown pane subcommand: %s", subArgs[0])
	}
}

func runStackedPaneCommand(store *session.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("stacked-pane requires a subcommand: list, create")
	}
	sess := flagVal(args, "--session")
	tabID := flagVal(args, "--tab")
	laneID := flagVal(args, "--lane")
	pgID := flagVal(args, "--pane-group")
	switch args[0] {
	case "list":
		return cli.StackedPaneList(store, sess, pgID)
	case "create":
		return cli.StackedPaneCreate(store, sess, tabID, laneID, pgID)
	default:
		return fmt.Errorf("unknown stacked-pane subcommand: %s", args[0])
	}
}

func runPipelineCommand(store *session.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("pipeline requires a subcommand: list, enable, disable")
	}
	sess := flagVal(args, "--session")
	tabID := flagVal(args, "--tab")
	switch args[0] {
	case "list":
		return cli.PipelineList(store, sess)
	case "enable":
		return cli.PipelineEnable(store, sess, tabID)
	case "disable":
		return cli.PipelineDisable(store, sess, tabID)
	default:
		return fmt.Errorf("unknown pipeline subcommand: %s", args[0])
	}
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	if abs, err := filepath.Abs(wd); err == nil {
		return abs
	}
	return wd
}

// daemonWorkDir resolves the working directory the daemon anchors per-workdir
// state under (.rysh/browser-instances etc.): cfg.WorkingDirectory when set,
// else the process cwd. Mirrors WorkspaceActor.browserWorkDir.
func daemonWorkDir(cfg config.Config) string {
	if wd := strings.TrimSpace(cfg.WorkingDirectory); wd != "" {
		return wd
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
