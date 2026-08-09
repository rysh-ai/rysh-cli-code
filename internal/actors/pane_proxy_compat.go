package actors

import (
	"log/slog"
	"path/filepath"

	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// Ungoverned-CLI detection wiring (design 022 §4.4).
//
// The proxy owns the observation (which panes have produced governed traffic);
// the pane owns the only thing that can see what is actually RUNNING. This file
// is the join: it resolves the foreground program's name and reports it, and it
// applies the per-CLI spawn adapters.

// proxyAdapterEnv returns the extra environment for CLIs the operator has opted
// into adapting. Nothing is returned for a CLI that base-URL injection already
// governs, or for one the operator has not opted in.
//
// The adapter environment is applied to the SHELL, so it is inherited by
// whichever agent CLI the user later runs in that pane. rysh cannot know in
// advance which one that will be, and an adapter that only took effect when the
// CLI was launched by rysh itself would miss the normal case entirely.
func (p *PaneActor) proxyAdapterEnv(base string) []string {
	var out []string
	for _, name := range proxy.KnownCLIs() {
		if !p.cfg.Proxy.AdapterEnabled(name) {
			continue
		}
		prof, ok := proxy.ProfileFor(name)
		if !ok {
			continue
		}
		dir := ""
		if p.cfg.RyshDir != "" {
			dir = filepath.Join(p.cfg.RyshDir, "proxy-adapters", name)
		}
		env, err := prof.AdaptSpawn(base+"/"+prof.Dialect+"/"+p.id, dir)
		if err != nil {
			// A broken adapter must not stop the pane from starting. The CLI
			// then runs unadapted — and the detector below is what says so.
			slog.Warn("proxy: adapter failed, CLI will run unadapted",
				"cli", name, "pane", p.id, "err", err)
			continue
		}
		out = append(out, env...)
	}
	return out
}

// noteForegroundProgram tells the proxy which program the pane is running, so a
// known agent CLI that never produces governed traffic can be reported.
//
// pgid is the PTY's foreground process group, which doubles as the group
// leader's pid. A pgid at or below the shell's own means the shell is in front
// and there is nothing to watch.
func (p *PaneActor) noteForegroundProgram(pgid int) {
	srv := p.proxyServer()
	if srv == nil {
		return
	}
	if pgid <= 0 || (p.shellPgid > 0 && pgid == p.shellPgid) {
		srv.NoteForeground(p.id, "")
		return
	}
	srv.NoteForeground(p.id, processName(pgid))
}

// emitUnproxiedWarning writes the one-shot warning for a pane whose known CLI
// has run past the grace window without a governed request.
//
// Warn-only by DEFAULT, by design. This is negative evidence: an idle CLI is
// indistinguishable from one that escaped the proxy, so blocking on it would
// punish the innocent case. `##proxy check` is the deterministic answer.
//
// Under `[proxy] strict` (or policy `proxy.strict`) the operator has said they
// would rather stop an innocent CLI than let an ungoverned one keep talking to
// a provider — design 022 §8.2's "honest v2 shape" — so the warning is followed
// by stopping the program.
func (p *PaneActor) emitUnproxiedWarning() {
	srv := p.proxyServer()
	if srv == nil {
		return
	}
	bin, ok := srv.DueWarning(p.id)
	if !ok {
		return
	}
	_ = p.pub.SendPaneRyshOutput(p.id, proxy.UnproxiedWarning(bin))
	if !srv.Strict() {
		return
	}
	_ = p.pub.SendPaneRyshOutput(p.id, proxy.StrictBlockNotice(bin))
	p.stopUngovernedProgram(bin)
}

// stopUngovernedProgram terminates the pane's foreground process group under
// strict mode.
//
// SIGTERM, not SIGKILL: an agent CLI holds a session, a scratch directory and
// often a half-written file, and rysh stopping it must not be worse for the
// user than the CLI stopping itself. A CLI that ignores SIGTERM keeps running
// and the user has been told twice why that is a problem — which is still
// better than rysh corrupting their working tree to prove a point.
//
// The signal goes to the process GROUP (-pgid), because an agent CLI is
// typically a node process with children of its own, and terminating only the
// leader would leave those children talking to the provider.
func (p *PaneActor) stopUngovernedProgram(binary string) {
	pgid := foregroundPgrp(p.ptyFile)
	if pgid <= 0 || (p.shellPgid > 0 && pgid == p.shellPgid) {
		// The program already went away, or the shell is back in front. Nothing
		// to stop, and signalling the shell's own group would kill the pane.
		slog.Info("proxy strict: nothing in the foreground to stop",
			"pane", p.id, "cli", binary, "pgid", pgid)
		return
	}
	if err := terminateProcessGroup(pgid); err != nil {
		slog.Warn("proxy strict: could not stop the ungoverned CLI",
			"pane", p.id, "cli", binary, "pgid", pgid, "err", err)
		_ = p.pub.SendPaneRyshOutput(p.id,
			"[proxy] strict: could not stop '"+binary+"': "+err.Error()+"\n")
		return
	}
	slog.Warn("proxy strict: stopped an ungoverned CLI",
		"pane", p.id, "cli", binary, "pgid", pgid)
}

// proxyServer reaches the running governance proxy. It is a process global for
// exactly the reason endpoint.go documents: one proxy per session daemon, read
// from deep in the actor hierarchy, and threading it through every constructor
// would be far more invasive than the atomic it replaces.
func (p *PaneActor) proxyServer() *proxy.Server { return proxy.Current() }

// processName resolves a pid to its executable name. The implementation is
// per-OS (pane_procname_*.go): Linux reads /proc/<pid>/comm, darwin asks the
// kernel for the process's kinfo_proc, and anything else returns "" so that
// detection degrades to silence rather than to a wrong answer.
//
// It used to be the /proc read alone, unguarded by a build tag, which meant it
// returned "" for every pid on macOS — and since an empty name clears the
// govWatch observation (noteForeground) and blanks the pane's foreground
// program, both `[proxy] strict` and `##pane list`'s "running" column were dead
// on the whole platform. See F-12.
