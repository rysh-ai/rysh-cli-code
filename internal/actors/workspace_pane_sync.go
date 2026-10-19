// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ##pane sync — whether a pane's live repaint stream reaches web clients.
//
//	##pane sync                                show this pane's web sync
//	##pane sync on|off [--pane <ref>]          resume / pause one pane
//	##pane sync off --all [--except <ref>]...  pause every pane but the caller
//	##pane sync off --tab <tab> [--except ...] pause every pane of one tab
//	##pane sync on --all | --tab <tab>         resume them again
//	##pane sync list                           list the paused panes
//
// Paused means the web server stops sending the pane's VT frames and output to
// browsers (web/pane_sync.go); the pane itself keeps running and keeps its
// place in the layout. It is stored as pane meta web.sync=off, so it survives a
// daemon restart and fleet tooling can stamp it on the panes it launches.
//
// The bulk forms never pause the calling pane — that is the orchestrator the
// user is driving the fleet from — nor an agents-board pane, which has no
// repaint stream to pause (it is fed by board posts) and is the other surface
// the user watches. Either can still be paused by naming it with --pane.

var paneSyncUsage = []string{
	"##pane sync                                show this pane's web sync (on = browsers repaint it live)",
	"##pane sync on|off [--pane <ref>]          resume / pause one pane's repaint stream to web clients",
	"##pane sync off --all [--except <ref>]...  pause every pane except this one and agents boards",
	"##pane sync off --tab <tab> [--except <ref>]...  the same, for the panes of one tab",
	"##pane sync on --all | --tab <tab>         resume them",
	"##pane sync list                           list the paused panes",
}

func (w *WorkspaceActor) handlePaneSyncCommand(out *strings.Builder, paneID string, args []string) {
	args, all := extractBoolArg(args, "--all")
	args, excepts := extractRepeatedArg(args, "--except")
	tabArg := extractTabFlag(args)
	args = dropFlagWithValue(args, "--tab", "-t")
	rest, targetRef := splitPaneTarget(args, "")

	verb := ""
	if len(rest) > 0 {
		verb = strings.ToLower(rest[0])
	}
	if len(rest) > 1 {
		ryshWriter(out).Usage(paneSyncUsage...)
		w.failRyshUsage("unexpected argument for ##pane sync: %q", rest[1])
		return
	}
	bulk := all || tabArg != ""

	switch verb {
	case "list", "ls":
		w.paneSyncList(out)
		return
	case "", "status":
		if bulk || len(excepts) > 0 {
			ryshWriter(out).Usage(paneSyncUsage...)
			w.failRyshUsage("--all/--tab/--except need on or off")
			return
		}
		target, ok := w.paneSyncTarget(out, targetRef, paneID)
		if !ok {
			return
		}
		state := "on"
		if domain.WebSyncPaused(w.paneMeta(target)) {
			state = "off (web clients hold its last screen)"
		}
		fmt.Fprintf(out, "\n[rysh] pane %.8s web sync: %s\n", target, state)
		return
	case "on", "off":
	default:
		ryshWriter(out).Unknown("pane sync", verb, paneSyncUsage...)
		w.failRyshUsage("unknown subcommand for ##pane sync: %q", verb)
		return
	}
	off := verb == "off"

	if !bulk {
		if len(excepts) > 0 {
			ryshWriter(out).Usage(paneSyncUsage...)
			w.failRyshUsage("--except needs --all or --tab")
			return
		}
		target, ok := w.paneSyncTarget(out, targetRef, paneID)
		if !ok {
			return
		}
		w.setPaneSync(target, off)
		fmt.Fprintf(out, "\n[rysh] pane %.8s web sync %s\n", target, verb)
		return
	}
	if targetRef != "" {
		ryshWriter(out).Usage(paneSyncUsage...)
		w.failRyshUsage("--pane cannot be combined with --all or --tab")
		return
	}

	// Bulk: every pane of the workspace, or of one tab.
	var tabs []*tabInfo
	if all {
		tabs = w.tabs
	} else {
		tab := w.resolveTabArg(tabArg)
		if tab == nil {
			fmt.Fprintf(out, "\n[rysh] tab not found: %s\n", tabArg)
			w.failRysh("tab not found: %s", tabArg)
			return
		}
		tabs = []*tabInfo{tab}
	}
	skip := map[string]bool{}
	for _, ref := range excepts {
		id := w.resolvePaneID(ref)
		if id == "" {
			fmt.Fprintf(out, "\n[rysh] pane not found: %s\n", ref)
			w.failRysh("pane not found: %s", ref)
			return
		}
		skip[id] = true
	}
	changed, kept := 0, 0
	for _, t := range tabs {
		snap := w.queryTabSnapshot(t.id)
		if snap == nil {
			fmt.Fprintf(out, "\n[rysh] could not fetch the snapshot of tab %s — its panes were left as they were\n", t.title)
			w.failRysh("could not fetch tab snapshot: %s", t.title)
			continue
		}
		for p := range domain.PanesInTab(snap) {
			if off && (skip[p.ID] || p.ID == paneID || p.PaneType == domain.PaneTypeAgentsBoard) {
				kept++
				continue
			}
			if domain.WebSyncPaused(p.Meta) == off {
				continue // already in the requested state
			}
			w.setPaneSync(p.ID, off)
			changed++
		}
	}
	if off {
		fmt.Fprintf(out, "\n[rysh] web sync off for %d pane(s); %d kept live (this pane, agents boards, --except)\n", changed, kept)
	} else {
		fmt.Fprintf(out, "\n[rysh] web sync on for %d pane(s)\n", changed)
	}
}

// paneSyncTarget resolves --pane (or the calling pane) to a pane id.
func (w *WorkspaceActor) paneSyncTarget(out *strings.Builder, ref, paneID string) (string, bool) {
	if ref == "" || ref == paneID {
		if paneID == "" {
			fmt.Fprintf(out, "\n[rysh] no active pane\n")
			w.failRysh("no active pane")
			return "", false
		}
		return paneID, true
	}
	id := w.resolvePaneID(ref)
	if id == "" {
		fmt.Fprintf(out, "\n[rysh] pane not found: %s\n", ref)
		w.failRysh("pane not found: %s", ref)
		return "", false
	}
	return id, true
}

// setPaneSync writes web.sync. Resuming DELETES the key rather than writing
// "on": the absent key is the default, and it keeps `##pane meta list` quiet
// for every pane nobody ever paused.
func (w *WorkspaceActor) setPaneSync(paneID string, off bool) {
	value := ""
	if off {
		value = domain.WebSyncOff
	}
	_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
		&msg.MsgPaneSetMeta{Key: domain.PaneMetaWebSync, Value: value})
}

func (w *WorkspaceActor) paneSyncList(out *strings.Builder) {
	type row struct{ tab, id, name string }
	var rows []row
	for _, t := range w.tabs {
		snap := w.queryTabSnapshot(t.id)
		if snap == nil {
			continue
		}
		for p := range domain.PanesInTab(snap) {
			if !domain.WebSyncPaused(p.Meta) {
				continue
			}
			name := p.GivenName
			if name == "" {
				name = p.Title
			}
			rows = append(rows, row{t.title, p.ID, name})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].tab != rows[j].tab {
			return rows[i].tab < rows[j].tab
		}
		return rows[i].name < rows[j].name
	})
	fmt.Fprintf(out, "\n[rysh] panes with web sync off (%d)\n", len(rows))
	ryshWriter(out).Rule()
	if len(rows) == 0 {
		fmt.Fprintf(out, "  (none — every pane repaints live in web clients)\n")
	}
	for _, r := range rows {
		fmt.Fprintf(out, "  %-16s %.8s  %s\n", r.tab, r.id, r.name)
	}
	ryshWriter(out).Rule()
}

// extractRepeatedArg removes every `flag <value>` / `flag=<value>` pair from
// args and returns the values in order.
func extractRepeatedArg(args []string, flag string) (rest, values []string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == flag && i+1 < len(args):
			values = append(values, args[i+1])
			i++
		case strings.HasPrefix(args[i], flag+"="):
			values = append(values, strings.TrimPrefix(args[i], flag+"="))
		default:
			rest = append(rest, args[i])
		}
	}
	return rest, values
}

// dropFlagWithValue removes every `flag <value>` / `flag=<value>` pair for
// each of flags. extractTabFlag reads --tab (or -t) without consuming it.
func dropFlagWithValue(args []string, flags ...string) []string {
	for _, f := range flags {
		args, _ = extractRepeatedArg(args, f)
	}
	return args
}
