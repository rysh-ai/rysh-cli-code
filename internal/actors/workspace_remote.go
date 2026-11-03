// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/remote"
)

// remoteHostEntry is one `##remote add` of this workspace.
type remoteHostEntry struct {
	alias   string
	session string
	opts    remote.SSHOptions
	pid     *actor.PID
	state   string
	note    string
	sshPID  int
}

const remoteUsage = "##remote add <ssh-host-alias> [--session <name>] [--dir <remote workspace>] [--config <remote rysh.config.yaml>] [--bin <remote rysh binary>] [--ssh-config <file>]"

// handleRemoteCommand implements ##remote (feature #14, design 070).
func (w *WorkspaceActor) handleRemoteCommand(out *strings.Builder, args []string) error {
	o := ryshWriter(out)
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "add":
		return w.remoteAdd(out, args[1:])
	case "list", "ls":
		w.remoteList(out)
		return nil
	case "remove", "rm":
		if len(args) != 2 {
			o.Usage("##remote remove <ssh-host-alias>")
			return errors.New("usage: ##remote remove <ssh-host-alias>")
		}
		return w.remoteRemove(out, args[1])
	case "reconnect":
		if len(args) != 2 {
			o.Usage("##remote reconnect <ssh-host-alias>")
			return errors.New("usage: ##remote reconnect <ssh-host-alias>")
		}
		e := w.remoteHosts[args[1]]
		if e == nil {
			return fmt.Errorf("no remote host %q (see ##remote list)", args[1])
		}
		w.system().Root.Send(e.pid, &remoteHostReconnect{})
		fmt.Fprintf(out, "\n[rysh] reconnecting to %s\n", e.alias)
		return nil
	}
	o.Usage(remoteUsage, "##remote list", "##remote remove <ssh-host-alias>", "##remote reconnect <ssh-host-alias>")
	return fmt.Errorf("unknown ##remote subcommand %q", args[0])
}

// parseRemoteAdd turns `##remote add` arguments into ssh options, validating
// them exactly as the connection will use them. Nothing is dialed here.
func parseRemoteAdd(args []string) (remote.SSHOptions, error) {
	var o remote.SSHOptions
	if len(args) == 0 {
		return o, errors.New("usage: " + remoteUsage)
	}
	o.Host = args[0]
	rest := args[1:]
	for len(rest) > 0 {
		if len(rest) < 2 {
			return o, fmt.Errorf("%s needs a value", rest[0])
		}
		v := rest[1]
		switch rest[0] {
		case "--session":
			o.Session = v
		case "--dir":
			o.RemoteDir = v
		case "--config":
			o.RemoteConfig = v
		case "--bin":
			o.RemoteBinary = v
		case "--ssh-config":
			o.ConfigFile = v
		default:
			return o, fmt.Errorf("unknown flag %s; usage: %s", rest[0], remoteUsage)
		}
		rest = rest[2:]
	}
	if _, err := remote.SSHArgs(o); err != nil {
		return o, err
	}
	return o, nil
}

func (w *WorkspaceActor) remoteAdd(out *strings.Builder, args []string) error {
	opts, err := parseRemoteAdd(args)
	if err != nil {
		ryshWriter(out).Usage(remoteUsage)
		return err
	}
	if _, dup := w.remoteHosts[opts.Host]; dup {
		return fmt.Errorf("remote host %q is already added (##remote remove %s first)", opts.Host, opts.Host)
	}
	if w.system() == nil {
		return errors.New("##remote add: workspace not running")
	}
	ha := NewRemoteHostActor(opts.Host, opts, w.pub, w.nc, w.selfPID)
	pid := w.system().Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return ha }))
	if w.remoteHosts == nil {
		w.remoteHosts = map[string]*remoteHostEntry{}
	}
	w.remoteHosts[opts.Host] = &remoteHostEntry{alias: opts.Host, session: opts.Session, opts: opts, pid: pid, state: remoteStateConnecting}
	args2, _ := remote.SSHArgs(opts)
	fmt.Fprintf(out, "\n[rysh] connecting to %s: ssh %s\n", opts.Host, strings.Join(args2, " "))
	fmt.Fprintf(out, "[rysh] its tabs appear here as ⇄ %s tabs; ##remote list shows the state\n", opts.Host)
	return nil
}

func (w *WorkspaceActor) remoteRemove(out *strings.Builder, alias string) error {
	e := w.remoteHosts[alias]
	if e == nil {
		return fmt.Errorf("no remote host %q (see ##remote list)", alias)
	}
	if w.system() != nil && e.pid != nil {
		w.system().Root.Stop(e.pid)
	}
	delete(w.remoteHosts, alias)
	for _, mt := range append([]*mirrorTab(nil), w.mirrorTabs...) {
		if mt.host == alias {
			w.removeMirrorTab(mt.shareID)
		}
	}
	w.invalidateSnapshotCaches()
	w.notifyMirrorDirty("")
	fmt.Fprintf(out, "\n[rysh] removed remote host %s (its panes keep running on the remote)\n", alias)
	return nil
}

func (w *WorkspaceActor) remoteList(out *strings.Builder) {
	o := ryshWriter(out)
	o.Header("remote hosts")
	if len(w.remoteHosts) == 0 {
		o.Row("none — %s", remoteUsage)
		return
	}
	names := make([]string, 0, len(w.remoteHosts))
	for n := range w.remoteHosts {
		names = append(names, n)
	}
	sort.Strings(names)
	o.Row("%-24s %-16s %-13s %4s %7s  %s", "HOST", "SESSION", "STATE", "TABS", "SSH-PID", "NOTE")
	for _, n := range names {
		e := w.remoteHosts[n]
		sess := e.session
		if sess == "" {
			sess = "(default)"
		}
		tabs := 0
		for _, mt := range w.mirrorTabs {
			if mt.host == n {
				tabs++
			}
		}
		pid := "-"
		if e.sshPID > 0 {
			pid = fmt.Sprint(e.sshPID)
		}
		o.Row("%-24s %-16s %-13s %4d %7s  %s", n, sess, e.state, tabs, pid, e.note)
	}
}

// applyRemoteHostLayout makes the workspace's tabs for a host match the
// host's current tab list: new remote tabs appear as control-mode mirror tabs
// fed by the host actor, known ones update, vanished ones go.
func (w *WorkspaceActor) applyRemoteHostLayout(m *MsgRemoteHostLayout) {
	e := w.remoteHosts[m.Host]
	if e == nil {
		return // removed meanwhile, or never added
	}
	seen := map[string]bool{}
	for _, rt := range m.Tabs {
		seen[rt.ShareID] = true
		mt := w.mirrorTabByShareID(rt.ShareID)
		if mt == nil {
			mt = w.addMirrorTab(rt.ShareID, rt.Title)
			mt.mode = "control"
			mt.host = m.Host
			mt.listenerPID = e.pid
		}
		mt.hostState = e.state
		w.applyMirrorTabUpdate(&MsgMirrorTabUpdate{ShareID: rt.ShareID, Alias: rt.Title, Tab: rt.Tab, Deltas: rt.Deltas})
	}
	removed := false
	for _, mt := range append([]*mirrorTab(nil), w.mirrorTabs...) {
		if mt.host == m.Host && !seen[mt.shareID] {
			w.removeMirrorTab(mt.shareID)
			removed = true
		}
	}
	if removed {
		w.invalidateSnapshotCaches()
		w.notifyMirrorDirty("")
	}
}

// applyRemoteHostStatus records a host's state and shows it on its tabs.
func (w *WorkspaceActor) applyRemoteHostStatus(m *MsgRemoteHostStatus) {
	e := w.remoteHosts[m.Host]
	if e == nil {
		return
	}
	stateChanged := e.state != m.State
	e.state = m.State
	if stateChanged {
		e.sshPID = m.SSHPID
	}
	switch {
	case m.Note != "":
		e.note = m.Note
	case m.State == remoteStateConnecting:
		// A retry keeps the reason of the drop on screen until it succeeds.
	case stateChanged:
		e.note = ""
	}
	for _, mt := range w.mirrorTabs {
		if mt.host == m.Host {
			mt.hostState = e.state
			mt.hostNote = e.note
			w.notifyMirrorDirty(mt.shareID)
		}
	}
	w.invalidateSnapshotCaches()
}

// remoteAddArgs turns a configured host into `##remote add` arguments.
func remoteAddArgs(h config.RemoteHostConfig) []string {
	args := []string{h.Alias}
	for _, kv := range [][2]string{{"--session", h.Session}, {"--dir", h.Dir}, {"--config", h.Config}, {"--bin", h.Bin}, {"--ssh-config", h.SSHConfig}} {
		if kv[1] != "" {
			args = append(args, kv[0], kv[1])
		}
	}
	return args
}

// addConfiguredRemoteHosts adds every `remote: hosts:` entry. A bad entry is
// logged and skipped; it never stops the session from starting.
func (w *WorkspaceActor) addConfiguredRemoteHosts() {
	for _, h := range w.cfg.RemoteHosts {
		var out strings.Builder
		if err := w.remoteAdd(&out, remoteAddArgs(h)); err != nil {
			slog.Warn("remote: configured host skipped", "host", h.Alias, "err", err)
		}
	}
}

// stopRemoteHosts stops every host actor (workspace shutdown).
func (w *WorkspaceActor) stopRemoteHosts() {
	for n, e := range w.remoteHosts {
		if e.pid != nil && w.system() != nil {
			w.system().Root.Stop(e.pid)
		}
		delete(w.remoteHosts, n)
	}
}
