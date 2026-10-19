// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// cmdSelectors pins specific entities for a ##cmd broadcast. An empty field
// means "use the active entity at that level".
type cmdSelectors struct {
	ws   string
	tab  string
	lane string
	pg   string
	pane string
	// running narrows the resolved scope to panes whose FOREGROUND program is
	// this executable ("claude", "vim"). It is a filter, not a target: the scope
	// still says where to look, this says which of those panes to touch. Sending
	// a shell command to a pane running a full-screen program types into that
	// program, so being able to say "only the ones at a shell" — or "only the
	// ones running claude" — is the difference between a broadcast and a mess.
	running string
	// runningImplied records that `running` was not asked for but supplied by
	// --capture. It changes nothing about the filtering; it is what lets the
	// summary explain a skip the caller did not ask for.
	runningImplied bool
	// capture redirects each pane's output to a file instead of leaving it on
	// the pane's screen, and reports where. See captureCommand.
	capture bool
}

// cmdSkips counts the panes a broadcast resolved but refused to touch. Every
// one of these is a pane the caller asked for and did not get, so the summary
// reports each reason: a fan-out that quietly lands on fewer panes than it was
// given is indistinguishable from one that worked.
type cmdSkips struct {
	shared   int
	pipeline int
	// busy is the count dropped by --running, whether asked for or implied.
	busy int
}

// parseCmdArgs parses the arguments of `##cmd`:
//
//	##cmd <scope> [--ws v] [--tab v] [--lane v] [--pg v] [--pane v] [--] <bash command...>
//
// <scope> is one of pane | panegroup|pg|stack | lane | tab | workspace|ws.
// Flag parsing stops at the first non-flag token (or an explicit "--");
// everything after that is the bash command. Scope aliases are normalized
// ("pg" and "stack" → "panegroup", "ws" → "workspace"). "stack" is offered
// because a pane group is exactly a stack of panes.
func parseCmdArgs(args []string) (scope string, sel cmdSelectors, command string, err error) {
	if len(args) == 0 {
		return "", sel, "", fmt.Errorf("usage: ##cmd <pane|pg|lane|tab|ws> [selectors] <command>")
	}
	scope = strings.ToLower(args[0])
	switch scope {
	case "pane", "panegroup", "pg", "stack", "lane", "tab", "workspace", "ws":
	default:
		return "", sel, "", fmt.Errorf("unknown scope %q (expected pane, pg/stack, lane, tab, or ws)", args[0])
	}

	rest := args[1:]
	i := 0
	for i < len(rest) {
		tok := rest[i]
		if tok == "--" {
			i++
			break
		}
		if !strings.HasPrefix(tok, "--") {
			break // first non-flag token: the command starts here
		}
		// Boolean flags take no value; checked before the pair parsing below so
		// "--capture pwd" does not swallow the command as --capture's argument.
		if tok == "--capture" {
			sel.capture = true
			i++
			continue
		}
		if i+1 >= len(rest) {
			return "", sel, "", fmt.Errorf("flag %s requires a value", tok)
		}
		val := rest[i+1]
		switch tok[2:] {
		case "ws", "workspace":
			sel.ws = val
		case "tab":
			sel.tab = val
		case "lane":
			sel.lane = val
		case "pg", "panegroup":
			sel.pg = val
		case "pane":
			sel.pane = val
		case "running":
			sel.running = val
		default:
			return "", sel, "", fmt.Errorf("unknown flag %s", tok)
		}
		i += 2
	}

	command = strings.TrimSpace(strings.Join(rest[i:], " "))
	if command == "" {
		return "", sel, "", fmt.Errorf("no command given; usage: ##cmd <scope> [selectors] <command>")
	}

	switch scope {
	case "pg", "stack":
		scope = "panegroup"
	case "ws":
		scope = "workspace"
	}

	// A CAPTURED COMMAND ONLY MAKES SENSE AT A SHELL. The redirection is shell
	// syntax: sent to a pane running claude (or vim, or a pager) it is not a
	// command at all, it is typed input, and no capture file is ever written.
	// Defaulting the filter here rather than trusting every caller to add
	// `--running shell` is what stops a --capture fan-out from becoming a
	// message to every agent in the scope — the failure that put 64 stray
	// prompts into three claude panes. An explicit --running still wins, so
	// `--running claude --capture` remains expressible for anyone who means it.
	if sel.capture && sel.running == "" {
		sel.running = "shell"
		sel.runningImplied = true
	}
	return scope, sel, command, nil
}

// ---------------------------------------------------------------------------
// Broadcast scope policy
//
// The generic tree navigation that used to live here — selector resolution,
// containment checks, pane collection and counting — has moved to
// internal/domain/navigate.go, where the ~18 other files that hand-roll the
// same walks can actually reach it. It was always general-purpose code; it was
// just parked in the one file that happened to need it first.
//
// What stays here is the part that is about ##cmd rather than about the tree:
// which panes a broadcast refuses to target.
// ---------------------------------------------------------------------------

// notShared rejects panes that are shared upstream with remote users. A
// broadcast never targets them — the command would execute in a pane a remote
// user is watching, and possibly driving.
func notShared(p *domain.PaneSnapshot) bool { return !p.Sharing }

// paneIDsInGroup/Lane/Tab return the IDs of every pane in the scope that a
// broadcast may target, along with the number excluded for being shared.
func paneIDsInGroup(g *domain.PaneGroupSnapshot) (ids []string, skipped int) {
	return domain.PaneIDsInGroupWhere(g, notShared)
}

func paneIDsInLane(l *domain.LaneSnapshot) (ids []string, skipped int) {
	return domain.PaneIDsInLaneWhere(l, notShared)
}

func paneIDsInTab(t *domain.TabSnapshot) (ids []string, skipped int) {
	return domain.PaneIDsInTabWhere(t, notShared)
}

// collectScopePaneIDs resolves the target pane IDs for a scope within snap,
// applying selector overrides (empty selector → active entity). Two classes of
// panes are excluded from the broadcast:
//   - panes shared upstream with remote users (Sharing == true) → skippedShared
//   - panes whose tab is a pipeline (TabSnapshot.PipelineEnabled) → skippedPipeline
//
// Pipeline membership is a tab-level property, so a pipeline-enabled tab excludes
// every pane in the requested scope. It also returns a human-readable label.
func collectScopePaneIDs(snap *domain.WorkspaceSnapshot, scope string, sel cmdSelectors) (ids []string, skippedShared, skippedPipeline int, label string, err error) {
	if scope == "workspace" {
		for i := range snap.Tabs {
			t := &snap.Tabs[i]
			if t.PipelineEnabled {
				skippedPipeline += domain.CountPanesInTab(t)
				continue
			}
			ti, ts := paneIDsInTab(t)
			ids = append(ids, ti...)
			skippedShared += ts
		}
		return ids, skippedShared, skippedPipeline, "the workspace", nil
	}
	tab := domain.ResolveTab(snap, sel.tab)
	if tab == nil {
		return nil, 0, 0, "", fmt.Errorf("tab not found: %q", sel.tab)
	}
	tabLabel := fmt.Sprintf("tab %q", tab.Title)
	if scope == "tab" {
		if tab.PipelineEnabled {
			return nil, 0, domain.CountPanesInTab(tab), tabLabel, nil
		}
		ids, skippedShared = paneIDsInTab(tab)
		return ids, skippedShared, 0, tabLabel, nil
	}
	lane := domain.ResolveLane(tab, sel.lane)
	if lane == nil {
		// A TAB WE COULD NOT READ IS NOT A TAB WITH NO LANES (`F-11`). The
		// snapshot marks a timed-out tab Partial; without this the two are
		// indistinguishable and the honest "I could not look" is reported as
		// the confident "it is not there" — the failure this whole track keeps
		// re-learning, here wearing a deterministic-looking error message.
		if tab.Partial {
			return nil, 0, 0, "", fmt.Errorf(
				"could not read %s: its snapshot timed out, so its lanes are unknown "+
					"(the tab and lane %q may well exist). Retry, or reduce the pane count",
				tabLabel, sel.lane)
		}
		return nil, 0, 0, "", fmt.Errorf("lane not found: %q", sel.lane)
	}
	laneLabel := fmt.Sprintf("lane %.8s of %s", lane.ID, tabLabel)
	if lane.Name != "" {
		laneLabel = fmt.Sprintf("lane %q of %s", lane.Name, tabLabel)
	}
	if scope == "lane" {
		if tab.PipelineEnabled {
			return nil, 0, domain.CountPanesInLane(lane), laneLabel, nil
		}
		ids, skippedShared = paneIDsInLane(lane)
		return ids, skippedShared, 0, laneLabel, nil
	}
	group := domain.ResolveGroup(lane, sel.pg)
	if group == nil {
		return nil, 0, 0, "", fmt.Errorf("pane group not found: %q", sel.pg)
	}
	groupLabel := fmt.Sprintf("pane group %.8s of %s", group.ID, laneLabel)
	if scope == "panegroup" {
		if tab.PipelineEnabled {
			return nil, 0, domain.CountPanesInGroup(group), groupLabel, nil
		}
		ids, skippedShared = paneIDsInGroup(group)
		return ids, skippedShared, 0, groupLabel, nil
	}
	// scope == "pane"
	pane := domain.ResolvePane(group, sel.pane)
	if pane == nil {
		return nil, 0, 0, "", fmt.Errorf("pane not found: %q", sel.pane)
	}
	paneLabel := fmt.Sprintf("pane %.8s", pane.ID)
	if tab.PipelineEnabled {
		// A pane in a pipeline tab never receives a broadcast command.
		return nil, 0, 1, paneLabel, nil
	}
	if pane.Sharing {
		// A shared pane never receives a broadcast command.
		return nil, 1, 0, paneLabel, nil
	}
	return []string{pane.ID}, 0, 0, paneLabel, nil
}

// ---------------------------------------------------------------------------
// WorkspaceActor methods
// ---------------------------------------------------------------------------

// anchorSnapshotToPane re-points the snapshot's "active" markers — the defaults
// used when a ##cmd selector at a given level is omitted — at the pane that
// issued the command. Explicit selectors resolve by id/name/index and are
// unaffected. This keeps ##cmd operating on the tab/lane/group the user actually
// issued it from, even when w.activeTabIdx (→ snap.ActiveTabID) has drifted out
// of sync with the focused pane. No-op when paneID is empty (e.g. a cross-
// workspace broadcast, where the originating pane lives in another workspace).
func anchorSnapshotToPane(snap *domain.WorkspaceSnapshot, paneID string) {
	if paneID == "" {
		return
	}
	for ti := range snap.Tabs {
		t := &snap.Tabs[ti]
		for li := range t.Lanes {
			l := &t.Lanes[li]
			for gi := range l.PaneGroups {
				g := &l.PaneGroups[gi]
				if domain.GroupContainsPane(g, paneID) {
					snap.ActiveTabID = t.ID
					t.ActivePaneID = paneID
					l.ActivePaneID = paneID
					g.ActivePaneID = paneID
					return
				}
			}
		}
	}
}

// broadcastCmd resolves the scope against this workspace's own snapshot and
// sends the bash command to every target pane. Panes shared upstream with remote
// users, and panes whose tab is a pipeline, are excluded (reported via
// skippedShared / skippedPipeline). Returns the count of panes the command was
// sent to, the two skip counts, a label, or an error. Used both for the active
// workspace (Phase 1) and, via the Farm, for a sibling workspace (Phase 2).
//
// anchorPaneID is the pane that issued the command; when set and the scope uses
// the active-entity default, resolution is anchored to that pane's tab/lane/group
// rather than the (possibly stale) active-tab index. Empty for cross-workspace
// broadcasts.
func (w *WorkspaceActor) broadcastCmd(scope string, sel cmdSelectors, command, anchorPaneID string) (targets []string, skips cmdSkips, label string, err error) {
	// LAYOUT ONLY, NO HISTORIES (`F-11`). This asks a purely STRUCTURAL question
	// — collectScopePaneIDs reads pane ids, Sharing and PipelineEnabled, and
	// filterPanesByProgram reads Program — and the full content snapshot of
	// every tab was missing its 2 s budget on a 111-pane tab. The timeout then
	// substituted a lane-less stub and the caller reported `lane not found`,
	// which sent every operator hunting a selector bug that did not exist.
	//
	// `--running` STILL WORKS: PaneActor.buildSnapshot sets Program
	// unconditionally (pane_snapshot.go), outside the includeContent guard, so
	// the cheap snapshot carries it. That was the precondition the F-11 report
	// demanded be confirmed before this line changed, and it is checked by
	// TestRunningFilterSurvivesTheCheapSnapshot.
	snap := w.collectSnapshot(true, true, "")
	anchorSnapshotToPane(&snap, anchorPaneID)
	ids, skippedShared, skippedPipeline, label, err := collectScopePaneIDs(&snap, scope, sel)
	if err != nil {
		return nil, cmdSkips{}, "", err
	}
	skips = cmdSkips{shared: skippedShared, pipeline: skippedPipeline}
	ids, skips.busy = filterPanesByProgram(&snap, ids, sel.running)
	if sel.capture {
		ensureCaptureDir(w.baseDir())
	}
	for _, id := range ids {
		sent := command
		if sel.capture {
			sent = captureCommand(w.baseDir(), id, command)
		}
		_ = w.pub.Send(msg.T("pane", id, "inbox"), &msg.MsgPaneExecShell{Command: sent})
	}
	return ids, skips, label, nil
}

// filterPanesByProgram narrows ids to panes whose foreground program matches,
// and returns how many it dropped. The empty filter matches everything; "shell"
// is spelled as the empty program, which is what a pane at its prompt reports,
// so `--running shell` means "the panes not busy with anything".
//
// The count is returned rather than discarded because this filter is now
// applied without being asked for (--capture implies --running shell). A pane
// silently missing from a fan-out is the one outcome the caller cannot detect.
func filterPanesByProgram(snap *domain.WorkspaceSnapshot, ids []string, want string) (kept []string, dropped int) {
	if want == "" {
		return ids, 0
	}
	if want == "shell" {
		want = ""
	}
	byID := map[string]string{}
	for _, tab := range snap.Tabs {
		for p := range domain.PanesInTab(&tab) {
			byID[p.ID] = p.Program
		}
	}
	out := ids[:0:0]
	for _, id := range ids {
		if byID[id] == want {
			out = append(out, id)
		}
	}
	return out, len(ids) - len(out)
}

// resolveCmdWorkspace interprets the --ws selector against the session's
// workspace list. Returns (targetName, isSibling, err):
//   - empty / the active workspace's own name / matching index → ("", false, nil): local.
//   - a known sibling (by name or 1-based index) → (name, true, nil): route via Farm.
//   - anything else → error.
func (w *WorkspaceActor) resolveCmdWorkspace(arg string) (string, bool, error) {
	if arg == "" {
		return "", false, nil
	}
	match := ""
	for _, n := range w.workspaceNames {
		if n == arg {
			match = n
			break
		}
	}
	if match == "" {
		if n, e := strconv.Atoi(arg); e == nil && n >= 1 && n <= len(w.workspaceNames) {
			match = w.workspaceNames[n-1]
		}
	}
	if match == "" {
		return "", false, fmt.Errorf("unknown workspace: %q", arg)
	}
	if match == w.workspaceName {
		return "", false, nil
	}
	return match, true, nil
}

// handleCmdBroadcast implements `##cmd <scope> [selectors] <bash command>`.
func (w *WorkspaceActor) handleCmdBroadcast(ctx actor.Context, out *strings.Builder, originPaneID string, args []string) {
	// `capture` is a read, not a scope: it serves back what an earlier
	// --capture wrote, without touching a pane. Intercepted before
	// parseCmdArgs, which would reject it as an unknown scope.
	if len(args) > 0 && strings.ToLower(args[0]) == "capture" {
		w.handleCmdCapture(out, args[1:])
		return
	}

	scope, sel, command, err := parseCmdArgs(args)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		w.cmdUsage(out)
		return
	}

	// Cross-workspace: route to a sibling workspace (same session) via the Farm.
	target, isSibling, rerr := w.resolveCmdWorkspace(sel.ws)
	if rerr != nil {
		w.failRysh("%v", rerr)
		fmt.Fprintf(out, "\n[rysh] %v\n", rerr)
		return
	}
	if isSibling {
		sel.ws = "" // consumed; sibling resolves the rest against its own snapshot
		ctx.Send(ctx.Parent(), &farmBroadcastCmdMsg{
			targetWorkspace: target,
			scope:           scope,
			sel:             sel,
			command:         command,
		})
		fmt.Fprintf(out, "\n[rysh] ##cmd: dispatched %q to workspace %q\n", command, target)
		return
	}

	// Local (active) workspace. Anchor active-entity defaults to the originating
	// pane so the command targets the tab/lane/group the user is actually in,
	// not the (possibly stale) active tab.
	targets, skips, label, err := w.broadcastCmd(scope, sel, command, originPaneID)
	count := len(targets)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return
	}
	if count == 0 {
		// The selector resolved but matched nothing, so the command ran
		// nowhere. A fan-out that silently hits zero panes is the worst
		// possible answer for a script: it looks like success.
		if sel.runningImplied && skips.busy > 0 {
			// Naming the implied filter matters most here: the scope DID
			// resolve panes, and they were dropped by a rule the caller never
			// typed. Without this the answer reads as "your selector is wrong".
			w.failRysh("##cmd matched no panes at a shell in %s: --capture targets "+
				"only panes at a prompt (%d busy); pass --running to override", label, skips.busy)
		} else {
			w.failRysh("##cmd matched no panes in %s", label)
		}
		fmt.Fprintf(out, "\n[rysh] %v\n", w.ryshFail)
		return
	}
	fmt.Fprintf(out, "\n[rysh] ##cmd: ran %q in %d pane(s) of %s", command, count, label)
	if skip := skipNote(skips, sel); skip != "" {
		fmt.Fprintf(out, " %s", skip)
	}
	fmt.Fprintf(out, "\n")
	if sel.capture {
		// The manifest, not the output: the commands are still running. A
		// reader polls each file for the sentinel — which is also how it learns
		// the exit status. Waiting here would block every other pane's commands
		// behind this one.
		fmt.Fprintf(out, "  capture: each pane writes to its own file; done when the last line is %s<status>\n",
			captureSentinel)
		for _, id := range targets {
			fmt.Fprintf(out, "  %s  %s\n", id, capturePath(w.baseDir(), id))
		}
	}
}

// skipNote formats the "(skipped …)" suffix for the ##cmd summary, listing only
// the nonzero exclusion reasons. sel supplies the wording for the --running
// skip, which reads differently depending on whether the caller asked for it.
func skipNote(s cmdSkips, sel cmdSelectors) string {
	var parts []string
	if s.shared > 0 {
		parts = append(parts, fmt.Sprintf("%d shared", s.shared))
	}
	if s.pipeline > 0 {
		parts = append(parts, fmt.Sprintf("%d pipeline", s.pipeline))
	}
	if s.busy > 0 {
		switch {
		case sel.running == "shell":
			parts = append(parts, fmt.Sprintf("%d not at a shell", s.busy))
		default:
			parts = append(parts, fmt.Sprintf("%d not running %s", s.busy, sel.running))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	note := "(skipped " + strings.Join(parts, ", ") + " pane(s))"
	if s.busy > 0 && sel.runningImplied {
		note += " — --capture targets only panes at a shell"
	}
	return note
}

// cmdUsage prints the usage block for ##cmd.
func (w *WorkspaceActor) cmdUsage(out *strings.Builder) {
	fmt.Fprintf(out, "  ##cmd <scope> [selectors] <bash command>\n")
	fmt.Fprintf(out, "    scope:     pane | pg/stack | lane | tab | ws\n")
	fmt.Fprintf(out, "    selectors: --ws --tab --lane --pg --pane <id|name|index>\n")
	fmt.Fprintf(out, "    filters:   --running <program|shell>   only panes running that program\n")
	fmt.Fprintf(out, "    capture:   --capture                   each pane's output to a file (prints where)\n")
	fmt.Fprintf(out, "                                           implies --running shell unless --running is given\n")
	fmt.Fprintf(out, "    read back: ##cmd capture [<pane>]      list captures, or print one (touches no pane)\n")
	fmt.Fprintf(out, "    examples:  ##cmd stack pwd   |   ##cmd tab --tab 2 git status   |   ##cmd ws --ws build make\n\n")
}
