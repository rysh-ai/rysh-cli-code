// SPDX-License-Identifier: Apache-2.0

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
//	##session model [<p>/<n>] show/bind the session-wide LLM model (== ##llm use)
//
// The returned error is the command's failure for a non-interactive caller
// (see ryshCommand.statusAware); the human-readable form is always written to
// out as well, so the pane reads exactly as it did before.
func (w *WorkspaceActor) handleSessionSubcommand(out *strings.Builder, paneID string, args []string) error {
	sub := "info"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "info":
		w.cmdSessionInfo(out)
	case "model", "llm":
		// Broadest scope in the hierarchy: every workspace/tab/lane/stack/pane
		// that binds nothing of its own runs this model.
		w.handleScopeModelCommand(out, paneID, scopeSession, args[1:])
	case "list", "ls":
		w.cmdSessionList(out)
	case "switch", "attach", "go":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		return w.cmdSessionSwitch(out, name)
	case "reload", "refresh":
		w.cmdSessionReload(out)
	default:
		w.sessionUsage(out)
		return fmt.Errorf("unknown ##session subcommand: %q", sub)
	}
	return nil
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
	fmt.Fprintf(out, "  front-end   : %s\n", session.FrontendName(w.cfg.SessionSource))

	var rec session.Record
	haveRec := false
	if store, err := session.NewStore(w.cfg); err == nil {
		if r, err := store.Get(name); err == nil {
			rec, haveRec = r, true
		}
	}
	if haveRec {
		fmt.Fprintf(out, "  created by  : %s\n", session.FrontendName(rec.Source))
		fmt.Fprintf(out, "  state       : %s\n", displaySessionState(rec))
		fmt.Fprintf(out, "  working dir : %s\n", rec.Path)
		fmt.Fprintf(out, "  daemon pid  : %d\n", rec.PID)
		fmt.Fprintf(out, "  nats port   : %d\n", rec.NATSPort)
		if rec.ProxyPort > 0 {
			fmt.Fprintf(out, "  proxy port  : %d (http://127.0.0.1:%d)\n", rec.ProxyPort, rec.ProxyPort)
		}
		if rec.WebPort > 0 {
			fmt.Fprintf(out, "  web port    : %d (http://127.0.0.1:%d — the desktop app adopts a session through this)\n",
				rec.WebPort, rec.WebPort)
			// How this session is reached from anywhere other than this
			// machine: the address it is bound to, the login it asks for, and
			// the public URL in front of it.
			if rec.WebHost != "" {
				fmt.Fprintf(out, "  web bind    : %s\n", rec.WebHost)
			}
			if rec.WebUser != "" {
				fmt.Fprintf(out, "  web login   : %s\n", rec.WebUser)
			}
			if rec.WebPublicURL != "" {
				fmt.Fprintf(out, "  web public  : %s\n", rec.WebPublicURL)
			}
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

	// When this front-end is opening a session the OTHER one created, list what
	// renders differently. The pre-attach print on stderr scrolls away with the
	// TUI's first repaint, so this is the durable copy: it stays available for
	// as long as the session is open, in the command a user already runs to ask
	// "what am I actually looking at".
	if haveRec {
		if notes, err := session.EnsureCanOpen(rec, w.cfg.SessionSource); err == nil && len(notes) > 0 {
			fmt.Fprintf(out, "\n  rendered here with:\n")
			for _, n := range notes {
				fmt.Fprintf(out, "    - %s\n", n)
			}
		}
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 56))
}

// displaySessionState renders a record's state for humans. The registry State
// field tracks TUI attachment only, so a healthy daemon reads "detached" even
// while the desktop app is connected — surface the live WebSocket-client
// presence (Record.AppClients) as "attached (app)" instead.
//
// Presence is keyed on AppClients alone, NOT on who created the session. It
// used to also require Source == "app", which was sound only while the two
// front-ends refused each other's sessions. Now that the app opens
// command-line sessions too, that extra condition would report a session as
// "detached" while the desktop app is visibly driving it.
func displaySessionState(r session.Record) string {
	if r.AppClients > 0 && r.PID > 0 && session.ProcessAlive(r.PID) {
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
		w.failRysh("could not open session registry: %v", err)
		return
	}
	records, err := store.List()
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] could not list sessions: %v\n", err)
		w.failRysh("could not list sessions: %v", err)
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
func (w *WorkspaceActor) cmdSessionSwitch(out *strings.Builder, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		ryshWriter(out).UsageLine("##session switch <name>   (see ##session list)")
		w.failRyshUsage("usage: %s", "##session switch <name>   (see ##session list)")
		return errors.New("##session switch: missing session name")
	}
	current := w.currentSessionName()
	if name == current {
		fmt.Fprintf(out, "\n[rysh] already on session %q\n", name)
		return nil
	}
	store, err := session.NewStore(w.cfg)
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] could not open session registry: %v\n", err)
		w.failRysh("could not open session registry: %v", err)
		return fmt.Errorf("open session registry: %w", err)
	}
	rec, err := store.Get(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "\n[rysh] session %q not found (see ##session list)\n", name)
			w.failRysh("session %q not found (see ##session list)", name)
			fmt.Fprintf(out, progname.Rewrite("  create it with:  rysh %s    (or: rysh create %s)\n"), name, name)
			return fmt.Errorf("session %q not found", name)
		}
		fmt.Fprintf(out, "\n[rysh] could not read session %q: %v\n", name, err)
		w.failRysh("could not read session %q: %v", name, err)
		return fmt.Errorf("read session %q: %w", name, err)
	}
	// A session created by the other front-end is switchable — it is the same
	// daemon behind it — but say up front which of its panes this front-end
	// cannot paint, before the user moves over and wonders.
	notes, mErr := session.EnsureCanOpen(rec, w.cfg.SessionSource)
	if mErr != nil {
		w.failRysh("%v", mErr)
		fmt.Fprintf(out, "\n[rysh] %v\n", mErr)
		return mErr
	}
	if len(notes) > 0 {
		fmt.Fprintf(out, "\n[rysh] %s was created by the %s; here:\n", name, session.FrontendName(rec.Source))
		for _, n := range notes {
			fmt.Fprintf(out, "  - %s\n", n)
		}
	}

	if !rec.DaemonAlive() {
		fmt.Fprintf(out, "\n[rysh] starting session %q daemon...\n", name)
		if err := session.SpawnDaemon(name, rec.Path, w.cfg.ConfigFile); err != nil {
			fmt.Fprintf(out, "  failed to start daemon: %v\n", err)
			w.failRysh("failed to start daemon: %v", err)
			return fmt.Errorf("start daemon for session %q: %w", name, err)
		}
		ready, werr := session.WaitReady(store, name, 3*time.Second)
		if werr != nil {
			fmt.Fprintf(out, "  daemon is still starting (%v)\n", werr)
			w.sessionAttachHelp(out, name, current)
			// Not an error: the daemon was spawned and is coming up. A script
			// that needs it ready should poll ##session list.
			return nil
		}
		rec = ready
	}

	fmt.Fprintf(out, "\n[rysh] session %q daemon running (pid %d, port %d)\n", name, rec.PID, rec.NATSPort)
	w.sessionAttachHelp(out, name, current)
	return nil
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
	ryshWriter(out).Usage(
		"##session              show current session details (alias: ##session info)",
		"##session list         list all known sessions (current marked with >)",
		"##session switch <name>  start another session's daemon + how to attach",
		"##session reload       flush this session's state to KV and refresh its record",
		"(set the working dir with ##workspace cwd <path>)",
	)
}
