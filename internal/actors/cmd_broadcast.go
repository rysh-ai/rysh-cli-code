package actors

import (
	"fmt"
	"os"
	"path/filepath"
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
	// capture redirects each pane's output to a file instead of leaving it on
	// the pane's screen, and reports where. See captureCommand.
	capture bool
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
func (w *WorkspaceActor) broadcastCmd(scope string, sel cmdSelectors, command, anchorPaneID string) (targets []string, skippedShared, skippedPipeline int, label string, err error) {
	snap := w.collectSnapshot(false, false)
	anchorSnapshotToPane(&snap, anchorPaneID)
	ids, skippedShared, skippedPipeline, label, err := collectScopePaneIDs(&snap, scope, sel)
	if err != nil {
		return nil, 0, 0, "", err
	}
	ids = filterPanesByProgram(&snap, ids, sel.running)
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
	return ids, skippedShared, skippedPipeline, label, nil
}

// filterPanesByProgram narrows ids to panes whose foreground program matches.
// The empty filter matches everything; "shell" is spelled as the empty program,
// which is what a pane at its prompt reports, so `--running shell` means "the
// panes not busy with anything".
func filterPanesByProgram(snap *domain.WorkspaceSnapshot, ids []string, want string) []string {
	if want == "" {
		return ids
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
	return out
}

// captureCommand rewrites a command so its output lands in a file instead of
// only on the pane's screen, and appends a sentinel line carrying the exit
// status so a reader can tell "still running" from "finished, no output".
//
// Why a file at all: a pane's output is a rendered terminal, not a stream. Once
// a full-screen program has drawn over it there is nothing to read back, and
// even at a shell prompt the text is wrapped to the pane's width. The file is
// the only faithful copy.
//
// The wait happens in the CALLER, deliberately. A ## command runs on the
// workspace's mailbox goroutine, which every other pane's commands queue behind
// — blocking it until N shells finish would freeze the session, which is
// exactly the defect an audit of `##proxy check` found in this codebase.
func captureCommand(baseDir, paneID, command string) string {
	path := capturePath(baseDir, paneID)
	// { … ; } groups without a subshell, so `cd` and exports behave as they
	// would unredirected. $? is read immediately after, before anything else
	// can overwrite it.
	return fmt.Sprintf("mkdir -p %s && { %s ; } > %s 2>&1; echo \"%s$?\" >> %s",
		filepath.Dir(path), command, path, captureSentinel, path)
}

// captureSentinel ends a captured run. The exit status follows it on the same
// line.
const captureSentinel = "__rysh_capture_done:"

func capturePath(baseDir, paneID string) string {
	return filepath.Join(baseDir, ".rysh", "captures", paneID+".out")
}

// ensureCaptureDir creates the capture directory and makes it invisible to git.
//
// Captures are transient output that lands inside the user's project, because
// that is where rysh keeps its state. Without this they show up as untracked
// files and ride along in the next `git add -A` — someone's build log committed
// as source. A directory that ignores itself is the smallest fix that survives
// being copied, moved or recreated.
func ensureCaptureDir(baseDir string) {
	dir := filepath.Join(baseDir, ".rysh", "captures")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	ignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(ignore); err == nil {
		return
	}
	_ = os.WriteFile(ignore, []byte("# rysh capture output — transient\n*\n"), 0o600)
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
	targets, skippedShared, skippedPipeline, label, err := w.broadcastCmd(scope, sel, command, originPaneID)
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
		w.failRysh("##cmd matched no panes in %s", label)
	}
	fmt.Fprintf(out, "\n[rysh] ##cmd: ran %q in %d pane(s) of %s", command, count, label)
	if skip := skipNote(skippedShared, skippedPipeline); skip != "" {
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
// the nonzero exclusion reasons.
func skipNote(shared, pipeline int) string {
	var parts []string
	if shared > 0 {
		parts = append(parts, fmt.Sprintf("%d shared", shared))
	}
	if pipeline > 0 {
		parts = append(parts, fmt.Sprintf("%d pipeline", pipeline))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(skipped " + strings.Join(parts, ", ") + " pane(s))"
}

// cmdUsage prints the usage block for ##cmd.
func (w *WorkspaceActor) cmdUsage(out *strings.Builder) {
	fmt.Fprintf(out, "  ##cmd <scope> [selectors] <bash command>\n")
	fmt.Fprintf(out, "    scope:     pane | pg/stack | lane | tab | ws\n")
	fmt.Fprintf(out, "    selectors: --ws --tab --lane --pg --pane <id|name|index>\n")
	fmt.Fprintf(out, "    filters:   --running <program|shell>   only panes running that program\n")
	fmt.Fprintf(out, "    capture:   --capture                   each pane's output to a file (prints where)\n")
	fmt.Fprintf(out, "    examples:  ##cmd stack pwd   |   ##cmd tab --tab 2 git status   |   ##cmd ws --ws build make\n\n")
}
