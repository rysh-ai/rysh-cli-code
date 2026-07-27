package actors

import (
	"errors"
	"fmt"
	"os"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// handleSessionSubcommand handles "##session ..." commands. Everything operates
// purely daemon-side through the on-disk session registry, so it behaves
// identically under the CLI and the desktop app (both drive the same rysh
// daemon):
//
//	##session [info]        show details about the current session
//	##session list          list all known sessions (current marked with ">")
//	##session switch <name> ensure another session's daemon is running + how to attach
//	##session reload        flush this session's state to KV and refresh its record
func (w *WorkspaceActor) handleSessionSubcommand(out *strings.Builder, args []string) {
	sub := "info"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "info":
		w.cmdSessionInfo(out)
	case "list", "ls":
		w.cmdSessionList(out)
	case "switch", "attach", "go":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		w.cmdSessionSwitch(out, name)
	case "reload", "refresh":
		w.cmdSessionReload(out)
	default:
		w.sessionUsage(out)
	}
}

// currentSessionName returns the name this daemon answers to (the NATS prefix),
// falling back to the configured name.
func (w *WorkspaceActor) currentSessionName() string {
	if w.sessionName != "" {
		return w.sessionName
	}
	return w.cfg.SessionName
}

// cmdSessionInfo prints details about the current session: its registry record
// (state, daemon pid, NATS port, attached TUIs, build) plus the live config
// locations and the current workspace shape.
func (w *WorkspaceActor) cmdSessionInfo(out *strings.Builder) {
	name := w.currentSessionName()

	fmt.Fprintf(out, "\n[rysh] session info\n")
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 56))
	fmt.Fprintf(out, "  name        : %s\n", name)
	fmt.Fprintf(out, "  source      : %s\n", session.NormalizeSource(w.cfg.SessionSource))

	var rec session.Record
	haveRec := false
	if store, err := session.NewStore(w.cfg); err == nil {
		if r, err := store.Get(name); err == nil {
			rec, haveRec = r, true
		}
	}
	if haveRec {
		fmt.Fprintf(out, "  state       : %s\n", displaySessionState(rec))
		fmt.Fprintf(out, "  working dir : %s\n", rec.Path)
		fmt.Fprintf(out, "  daemon pid  : %d\n", rec.PID)
		fmt.Fprintf(out, "  nats port   : %d\n", rec.NATSPort)
		if rec.ProxyPort > 0 {
			fmt.Fprintf(out, "  proxy port  : %d (http://127.0.0.1:%d)\n", rec.ProxyPort, rec.ProxyPort)
		}
		fmt.Fprintf(out, "  attached TUI: %d\n", len(rec.AliveTUIPIDs()))
		if rec.AppClients > 0 {
			fmt.Fprintf(out, "  app clients : %d\n", rec.AppClients)
		}
		if build := strings.TrimSpace(rec.Version + " " + rec.BinHash); build != "" {
			fmt.Fprintf(out, "  build       : %s\n", build)
		}
		fmt.Fprintf(out, "  updated     : %s\n", rec.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
	} else {
		fmt.Fprintf(out, "  state       : (no registry record found)\n")
		fmt.Fprintf(out, "  working dir : %s\n", w.cfg.WorkingDirectory)
	}

	// Config locations come from the live config, not the record.
	cfgFile := w.cfg.ConfigFile
	if cfgFile == "" {
		cfgFile = "(none)"
	}
	fmt.Fprintf(out, "  config file : %s\n", cfgFile)
	fmt.Fprintf(out, "  rysh dir    : %s\n", w.cfg.RyshDir)

	// Workspace shape (this daemon's live tabs/panes).
	totalPanes := 0
	for _, t := range w.tabs {
		totalPanes += w.queryPaneCount(t.id)
	}
	fmt.Fprintf(out, "  tabs        : %d\n", len(w.tabs))
	fmt.Fprintf(out, "  panes       : %d\n", totalPanes)
	if len(w.workspaceNames) > 1 {
		fmt.Fprintf(out, "  workspaces  : %d (active: %s)\n", len(w.workspaceNames), w.workspaceName)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 56))
}

// displaySessionState renders a record's state for humans. The registry State
// field tracks TUI attachment only, so a healthy app daemon reads "detached"
// even while the desktop app is connected — surface the live WebSocket-client
// presence (Record.AppClients) as "attached (app)" instead.
func displaySessionState(r session.Record) string {
	if session.NormalizeSource(r.Source) == "app" && r.AppClients > 0 &&
		r.PID > 0 && session.ProcessAlive(r.PID) {
		return "attached (app)"
	}
	return r.State
}

// cmdSessionList lists every known session in the registry, marking the current
// one with ">". It mirrors `rysh list-sessions` but renders into the pane.
func (w *WorkspaceActor) cmdSessionList(out *strings.Builder) {
	store, err := session.NewStore(w.cfg)
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] could not open session registry: %v\n", err)
		return
	}
	records, err := store.List()
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] could not list sessions: %v\n", err)
		return
	}
	current := w.currentSessionName()

	fmt.Fprintf(out, "\n[rysh] sessions (%d total)\n", len(records))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 72))
	if len(records) == 0 {
		fmt.Fprintf(out, "  (none)\n")
	}
	for _, r := range records {
		r.CleanStalePIDs()
		marker := "  "
		if r.Name == current {
			marker = "> "
		}
		fmt.Fprintf(out, "%s%-20s  %-14s  %-4s  tui=%-2d  %s\n",
			marker, r.Name, displaySessionState(r), session.NormalizeSource(r.Source), len(r.AliveTUIPIDs()), r.Path)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 72))
	fmt.Fprintf(out, "  switch with: ##session switch <name>\n")
}

// cmdSessionSwitch ensures another session's daemon is running (spawning it when
// stopped) and prints how to attach to it. Switching the visible front-end is
// the front-end's job (CLI: rysh attach; app: the session picker), so the daemon
// only guarantees the target is up and reachable.
func (w *WorkspaceActor) cmdSessionSwitch(out *strings.Builder, name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		fmt.Fprintf(out, "\n[rysh] usage: ##session switch <name>   (see ##session list)\n")
		return
	}
	current := w.currentSessionName()
	if name == current {
		fmt.Fprintf(out, "\n[rysh] already on session %q\n", name)
		return
	}
	store, err := session.NewStore(w.cfg)
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] could not open session registry: %v\n", err)
		return
	}
	rec, err := store.Get(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "\n[rysh] session %q not found (see ##session list)\n", name)
			fmt.Fprintf(out, progname.Rewrite("  create it with:  rysh %s    (or: rysh create %s)\n"), name, name)
		} else {
			fmt.Fprintf(out, "\n[rysh] could not read session %q: %v\n", name, err)
		}
		return
	}
	// Refuse to drive a session that belongs to the other front-end (the CLI and
	// the desktop app never share saved layouts).
	if mErr := session.EnsureSourceMatch(rec, w.cfg.SessionSource); mErr != nil {
		fmt.Fprintf(out, "\n[rysh] %v\n", mErr)
		return
	}

	if !rec.DaemonAlive() {
		fmt.Fprintf(out, "\n[rysh] starting session %q daemon...\n", name)
		if err := session.SpawnDaemon(name, rec.Path, w.cfg.ConfigFile); err != nil {
			fmt.Fprintf(out, "  failed to start daemon: %v\n", err)
			return
		}
		ready, werr := session.WaitReady(store, name, 3*time.Second)
		if werr != nil {
			fmt.Fprintf(out, "  daemon is still starting (%v)\n", werr)
			w.sessionAttachHelp(out, name, current)
			return
		}
		rec = ready
	}

	fmt.Fprintf(out, "\n[rysh] session %q daemon running (pid %d, port %d)\n", name, rec.PID, rec.NATSPort)
	w.sessionAttachHelp(out, name, current)
}

// sessionAttachHelp prints the front-end-specific steps to actually move to a
// (now-running) target session, plus a reminder that the current session stays
// up in the background.
func (w *WorkspaceActor) sessionAttachHelp(out *strings.Builder, target, current string) {
	fmt.Fprintf(out, progname.Rewrite("  attach from CLI:  rysh attach %s\n"), target)
	fmt.Fprintf(out, "  in the app:       use the session picker\n")
	fmt.Fprintf(out, progname.Rewrite("  (session %q keeps running in the background; detach with ctrl+o d or: rysh detach %s)\n"),
		current, current)
}

// cmdSessionReload flushes the live workspace state to JetStream KV immediately
// (bypassing the debounce gate) and refreshes the on-disk session record, so the
// persisted snapshot and registry match what is on screen right now.
func (w *WorkspaceActor) cmdSessionReload(out *strings.Builder) {
	name := w.currentSessionName()

	w.persistToKVNow()

	refreshed := false
	if store, err := session.NewStore(w.cfg); err == nil {
		if rec, err := store.Get(name); err == nil {
			if _, err := store.Upsert(rec); err == nil {
				refreshed = true
			}
		}
	}

	fmt.Fprintf(out, "\n[rysh] reloaded session %q\n", name)
	fmt.Fprintf(out, "  workspace state flushed to KV (%d tabs)\n", len(w.tabs))
	if refreshed {
		fmt.Fprintf(out, "  session registry record refreshed\n")
	}
}

// sessionUsage prints the ##session command help.
func (w *WorkspaceActor) sessionUsage(out *strings.Builder) {
	fmt.Fprintf(out, "\n[rysh] usage:\n")
	fmt.Fprintf(out, "  ##session              show current session details (alias: ##session info)\n")
	fmt.Fprintf(out, "  ##session list         list all known sessions (current marked with >)\n")
	fmt.Fprintf(out, "  ##session switch <name>  start another session's daemon + how to attach\n")
	fmt.Fprintf(out, "  ##session reload       flush this session's state to KV and refresh its record\n")
	fmt.Fprintf(out, "  (set the working dir with ##workspace cwd <path>)\n\n")
}
