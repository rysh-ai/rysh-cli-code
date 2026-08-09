package actors

import (
	"fmt"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handlePaneCommand implements the `##pane` command family: pane creation,
// listing, naming, cross-pane listening, sharing, provider/model overrides and
// deletion.

func (w *WorkspaceActor) handlePaneCommand(ctx actor.Context, out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		sub = "info"
	}
	switch sub {
	case "new":
		// ##pane new [--worktree [branch]] -> new pane; with --worktree
		// it runs in its own git worktree (design 008).
		w.handlePaneNewCommand(ctx, out, paneID, args[1:])

	case "list":
		// ##pane list [--tab <tab-id>]  -> list panes of a tab
		// (defaults to the active tab when --tab is omitted).
		listArgs, withMeta := extractBoolArg(args[1:], "--meta")
		tabArg := extractTabFlag(listArgs)
		tab := w.resolveTabArg(tabArg)
		if tab == nil {
			if tabArg == "" {
				fmt.Fprintf(out, "\n[rysh] no active tab\n")
				w.failRysh("no active tab")
			} else {
				fmt.Fprintf(out, "\n[rysh] tab not found: %s\n", tabArg)
				w.failRysh("tab not found: %s", tabArg)
			}
			break
		}
		w.cmdPaneListInTabWithMeta(out, tab, withMeta)

	case "info":
		// ##pane info              -> the ambient pane
		// ##pane info <pane-ref>   -> THAT pane, in whichever tab holds it
		//
		// F-16: this arm used to ignore args entirely. `##pane info <id>` read
		// w.currentTab() and the ambient paneID, so it answered — confidently,
		// in full, with no warning — about a pane the caller had not asked
		// about. It was found live: a query for pane 22299ff7 returned
		// f3a6c5cd, a different agent's pane, and nothing in the output said so.
		//
		// Three rules hold here, and each of them is a way the old behaviour was
		// wrong rather than merely incomplete:
		//
		//  1. REFUSE, DO NOT GUESS. An unresolvable ref is an error naming the
		//     ref. Falling back to the active pane is what made the defect
		//     invisible: a wrong answer that looks right is worse than a
		//     refusal.
		//  2. DO NOT FOCUS. A named pane may live in another tab, so this
		//     resolves the pane's OWN tab and searches that. It deliberately
		//     does NOT call focusPaneByID, which would switch the active tab and
		//     move the human's cursor — ##pane info is a read, and a read must
		//     not move anything.
		//  3. NO ARGUMENT KEEPS WORKING. Bare `##pane info` still reports the
		//     ambient pane. That is long-standing behaviour people rely on and
		//     turning it into an error would be a second defect, not a fix.
		target := paneID
		tab := w.currentTab()
		if ref := paneInfoRef(args); ref != "" {
			resolved := w.resolvePaneID(ref)
			if resolved == "" {
				fmt.Fprintf(out, "\n[rysh] pane not found: %s\n", ref)
				w.failRysh("pane not found: %s", ref)
				break
			}
			owning := w.findPaneTab(resolved)
			if owning == nil {
				// resolvePaneID saw it, so a tab holds it; not finding that tab
				// means a snapshot did not come back. Say that rather than
				// searching the active tab and reporting "not found" — or worse,
				// reporting the ambient pane.
				fmt.Fprintf(out, "\n[rysh] could not locate the tab holding pane: %s\n", ref)
				w.failRysh("could not locate the tab holding pane: %s", ref)
				break
			}
			target, tab = resolved, owning
		}
		if tab == nil {
			fmt.Fprintf(out, "\n[rysh] no active tab\n")
			w.failRysh("no active tab")
			break
		}
		if target == "" {
			fmt.Fprintf(out, "\n[rysh] no active pane\n")
			w.failRysh("no active pane")
			break
		}
		tabSnap := w.queryTabSnapshot(tab.id)
		if tabSnap == nil {
			fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
			w.failRysh("could not fetch tab snapshot")
			break
		}
		var ps *domain.PaneSnapshot
		var laneFlex int
		var laneID string
		for _, lane := range tabSnap.Lanes {
			for _, g := range lane.PaneGroups {
				for i := range g.Panes {
					if g.Panes[i].ID == target {
						ps = &g.Panes[i]
						laneFlex = lane.Flex
						laneID = lane.ID
						break
					}
				}
				if ps != nil {
					break
				}
			}
			if ps != nil {
				break
			}
		}
		if ps == nil {
			fmt.Fprintf(out, "\n[rysh] pane not found: %s\n", target)
			w.failRysh("pane not found: %s", target)
			break
		}
		givenNameDisplay := ps.GivenName
		if givenNameDisplay == "" {
			givenNameDisplay = "no-name"
		}
		fmt.Fprintf(out, "\n[rysh] pane info\n")
		ryshWriter(out).Rule()
		fmt.Fprintf(out, "  title      : %s\n", ps.Title)
		fmt.Fprintf(out, "  given-name : %s\n", givenNameDisplay)
		fmt.Fprintf(out, "  id         : %s\n", ps.ID)
		fmt.Fprintf(out, "  tab        : %s\n", tab.title)
		fmt.Fprintf(out, "  lane       : %.8s\n", laneID)
		fmt.Fprintf(out, "  status     : %s\n", ps.Status)
		fmt.Fprintf(out, "  mode       : %s\n", ps.Mode)
		fmt.Fprintf(out, "  provider   : %s\n", ps.ProviderName)
		if ps.Program != "" {
			fmt.Fprintf(out, "  running    : %s\n", ps.Program)
		}
		for _, k := range sortedMetaKeys(ps.Meta) {
			fmt.Fprintf(out, "  meta.%-6s : %s\n", k, ps.Meta[k])
		}
		fmt.Fprintf(out, "  lane flex  : %d\n", laneFlex)
		if ps.PTYRows > 0 && ps.PTYCols > 0 {
			// A pane on screen in more than one viewport is sized to the
			// smallest of them, so a large window can render a small grid with
			// space around it. Say so here, or that looks like a bug.
			if ps.SizeViewports > 1 {
				fmt.Fprintf(out, "  pty size   : %dx%d (smallest of %d viewports showing this pane)\n",
					ps.PTYCols, ps.PTYRows, ps.SizeViewports)
			} else {
				fmt.Fprintf(out, "  pty size   : %dx%d\n", ps.PTYCols, ps.PTYRows)
			}
		}
		if ps.LastCommand != "" {
			fmt.Fprintf(out, "  last cmd   : %s\n", ps.LastCommand)
		}
		ryshWriter(out).Rule()

	case "name":
		// ##pane name <given-name>            -> set given-name on the active pane
		// ##pane name <pane-id> <given-name>  -> set given-name on the pane with that id
		if len(args) < 2 {
			ryshWriter(out).Usage(
				"##pane name <given-name>             set the given-name of the active pane",
				"##pane name <pane-id> <given-name>   set the given-name of the pane with that id (see ##pane list)",
			)
			w.failRyshUsage("##pane name needs a name")
			break
		}
		// Default target is the active pane/tab; an explicit pane-id
		// first arg retargets to any pane in the workspace.
		targetPane := paneID
		targetTab := w.currentTab()
		givenName := args[1]
		if len(args) >= 3 {
			if t := w.findPaneTab(args[1]); t != nil {
				targetPane = args[1]
				targetTab = t
				givenName = args[2]
			}
		}
		if targetTab == nil {
			fmt.Fprintf(out, "\n[rysh] no active tab\n")
			w.failRysh("no active tab")
			break
		}
		if targetPane == "" {
			fmt.Fprintf(out, "\n[rysh] no active pane\n")
			w.failRysh("no active pane")
			break
		}
		// Check uniqueness within the target pane's lane.
		if targetTab.actor.IsGivenNameTakenInLane(targetPane, givenName) {
			fmt.Fprintf(out, "\n[rysh] error: given-name %q is already used by another pane in this lane\n", givenName)
			w.failRysh("error: given-name %q is already used by another pane in this lane", givenName)
			break
		}
		// Send directly to the pane, bypassing Tab/Lane/PaneGroup.
		_ = w.pub.Send(msg.T("pane", targetPane, "inbox"), &msg.MsgPaneSetGivenName{Name: givenName})
		fmt.Fprintf(out, "\n[rysh] pane %s given-name set to %q\n", targetPane, givenName)

	case "meta":
		// ##pane meta [--pane <id>] list
		// ##pane meta [--pane <id>] get <key>
		// ##pane meta [--pane <id>] set <key> <value...>
		// ##pane meta [--pane <id>] delete <key>
		//
		// Whoever drives a pane keeps its notes here — the session id of the
		// claude they started in it, the task it was opened for. It rides the
		// pane's KV record, so it survives a daemon restart and is legible to
		// every other tool, which a sidecar file in one tool's directory is not.
		rest, targetRef := splitPaneTarget(args[1:], paneID)
		target := targetRef
		if targetRef != "" && targetRef != paneID {
			// A ref may be a given-name or an auto-title; everything below
			// addresses the pane by id.
			if resolved := w.resolvePaneID(targetRef); resolved != "" {
				target = resolved
			} else {
				fmt.Fprintf(out, "\n[rysh] pane not found: %s\n", targetRef)
				w.failRysh("pane not found: %s", targetRef)
				break
			}
		}
		if target == "" {
			fmt.Fprintf(out, "\n[rysh] no active pane\n")
			w.failRysh("no active pane")
			break
		}
		sub := "list"
		if len(rest) > 0 {
			sub = strings.ToLower(rest[0])
		}
		snapMeta := w.paneMeta(target)
		switch sub {
		case "list", "ls", "":
			fmt.Fprintf(out, "\n[rysh] meta for pane %.8s (%d)\n", target, len(snapMeta))
			ryshWriter(out).Rule()
			if len(snapMeta) == 0 {
				fmt.Fprintf(out, "  (none — set one with ##pane meta set <key> <value>)\n")
			}
			for _, k := range sortedMetaKeys(snapMeta) {
				fmt.Fprintf(out, "  %-20s %s\n", k, snapMeta[k])
			}
			ryshWriter(out).Rule()
		case "get", "show":
			if len(rest) < 2 {
				ryshWriter(out).UsageLine("##pane meta get <key>")
				w.failRyshUsage("usage: ##pane meta get <key>")
				break
			}
			v, ok := snapMeta[rest[1]]
			if !ok {
				fmt.Fprintf(out, "\n[rysh] pane %.8s has no meta key %q\n", target, rest[1])
				w.failRysh("no meta key %q on pane %s", rest[1], target)
				break
			}
			fmt.Fprintf(out, "\n[rysh] %s = %s\n", rest[1], v)
		case "set":
			if len(rest) < 3 {
				ryshWriter(out).UsageLine("##pane meta set <key> <value>")
				w.failRyshUsage("usage: ##pane meta set <key> <value>")
				break
			}
			// The value keeps the remaining words: a ## line arrives already
			// split on whitespace, so a value with spaces would otherwise lose
			// everything after the first one.
			value := strings.Join(rest[2:], " ")
			_ = w.pub.Send(msg.T("pane", target, "inbox"), &msg.MsgPaneSetMeta{Key: rest[1], Value: value})
			fmt.Fprintf(out, "\n[rysh] pane %.8s meta %s = %s\n", target, rest[1], value)
		case "delete", "rm", "del", "unset":
			if len(rest) < 2 {
				ryshWriter(out).UsageLine("##pane meta delete <key>")
				w.failRyshUsage("usage: ##pane meta delete <key>")
				break
			}
			if _, ok := snapMeta[rest[1]]; !ok {
				fmt.Fprintf(out, "\n[rysh] pane %.8s has no meta key %q\n", target, rest[1])
				w.failRysh("no meta key %q on pane %s", rest[1], target)
				break
			}
			// Empty value is the delete: see MsgPaneSetMeta.
			_ = w.pub.Send(msg.T("pane", target, "inbox"), &msg.MsgPaneSetMeta{Key: rest[1]})
			fmt.Fprintf(out, "\n[rysh] pane %.8s meta %s deleted\n", target, rest[1])
		default:
			ryshWriter(out).Unknown("pane meta", sub,
				"##pane meta list             show this pane's metadata",
				"##pane meta get <key>        read one entry",
				"##pane meta set <key> <val>  write one entry",
				"##pane meta delete <key>     remove one entry",
				"  (all accept --pane <id> to target another pane)",
			)
			w.failRyshUsage("unknown subcommand for ##pane meta: %q", sub)
		}

	case "listen":
		if len(args) < 2 {
			ryshWriter(out).UsageLine("##pane listen <pane-id | pane-alias>")
			w.failRyshUsage("usage: %s", "##pane listen <pane-id | pane-alias>")
			break
		}
		target := args[1]
		targetID := w.resolvePaneID(target)
		if targetID == "" {
			fmt.Fprintf(out, "\n[rysh] pane not found: %s\n", target)
			w.failRysh("pane not found: %s", target)
			break
		}
		if targetID == paneID {
			fmt.Fprintf(out, "\n[rysh] cannot listen to self\n")
			w.failRysh("cannot listen to self")
			break
		}
		// Check for cyclic listening: if the target pane is already listening to this pane.
		if w.isPaneListeningTo(targetID, paneID) {
			fmt.Fprintf(out, "\n[rysh] cannot listen to pane %s: it is already listening to this pane (cyclic listening is not allowed)\n", target)
			w.failRysh("cannot listen to pane %s: it is already listening to this pane (cyclic listening is not allowed)", target)
			break
		}
		// Send directly to the pane, bypassing Tab/Lane/PaneGroup.
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgStartPaneListener{
			TargetPaneID: targetID,
			TargetAlias:  target,
		})

	case "unlisten":
		// Send directly to the pane, bypassing Tab/Lane/PaneGroup.
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgStopPaneListener{})

	case "share":
		action := ""
		if len(args) > 1 {
			action = args[1]
		}
		switch action {
		case "start", "":
			if paneID == "" {
				fmt.Fprintf(out, "\n[rysh] no active pane\n")
				w.failRysh("no active pane")
			} else {
				// Backward compat: also route through share registry if upstream enabled.
				if w.shareRegistryPID != nil {
					w.handleShareCommand(out, paneID, []string{"pane", "view"})
				} else {
					_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneShareStart{})
				}
			}
		case "stop":
			if paneID == "" {
				fmt.Fprintf(out, "\n[rysh] no active pane\n")
				w.failRysh("no active pane")
			} else {
				if w.shareRegistryPID != nil {
					w.handleUnshareCommand(out, paneID, []string{"pane"})
				} else {
					_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneShareStop{})
				}
			}
		case "status":
			if paneID == "" {
				fmt.Fprintf(out, "\n[rysh] no active pane\n")
				w.failRysh("no active pane")
			} else {
				if w.shareRegistryPID != nil {
					w.handleShareCommand(out, paneID, []string{"status"})
				} else {
					_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneShareStatus{})
					fmt.Fprintf(out, "\n[rysh] share status requested\n")
				}
			}
		default:
			ryshWriter(out).Usage(
				"##pane share start    start sharing active pane to upstream",
				"##pane share stop     stop sharing active pane",
				"##pane share status   show sharing status",
			)
			w.failRyshUsage("unknown ##pane share action")
		}

	case "provider":
		// ##pane provider [name [model]] — runtime provider override for
		// the ACTIVE pane (design 002 §3.4). Validation and output live in
		// cmdPaneProvider; the pane applies + persists on receipt.
		if m := w.cmdPaneProvider(out, paneID, args[1:]); m != nil {
			_ = w.pub.Send(msg.T("pane", paneID, "inbox"), m)
		}

	case "model", "llm":
		// ##pane model [<provider>/<name>] — same override seam as
		// ##pane provider, but addressed through the .rysh/llms registry
		// that ##llm lists, so a pane switches model by catalogue name.
		// Narrowest scope in the hierarchy: it outranks every level above.
		w.handleScopeModelCommand(out, paneID, scopePane, args[1:])

	case "approval-pane":
		w.handleApprovalPaneCommand(out, paneID, args[1:])

	case "delete":
		// ##pane delete <pane-id>  -> delete the pane with that id (see ##pane list)
		if len(args) < 2 {
			ryshWriter(out).UsageLine("##pane delete <pane-id>   (see ##pane list)")
			w.failRyshUsage("usage: %s", "##pane delete <pane-id>   (see ##pane list)")
		} else {
			resp := w.handleCLIDeletePane(ctx, &msg.MsgCLIDeletePane{PaneID: args[1]})
			if resp.OK {
				fmt.Fprintf(out, "\n[rysh] pane %s deleted\n", args[1])
			} else {
				fmt.Fprintf(out, "\n[rysh] %s\n", resp.Error)
				w.failRysh("%s", resp.Error)
			}
		}

	default:
		ryshWriter(out).Unknown("pane", sub,
			"##pane info",
			"##pane new [--worktree [branch]]",
			"##pane list [--tab <tab-id>]",
			"##pane name <name>",
			"##pane listen <id|alias>",
			"##pane unlisten",
			"##pane provider [name [model]]",
			"##pane model [<provider>/<name>|list|default]",
			"##pane delete <pane-id>",
			"##pane share start|stop|status",
			"##pane approval-pane <name1> [name2] ... | clear | list",
		)
		w.failRyshUsage("unknown subcommand for ##%s: %q", "pane", sub)
	}
}

// paneInfoRef returns the pane reference a `##pane info` invocation named, or ""
// when it named none.
//
// args[0] is the subcommand ("info"), so the reference is args[1]. Split out so
// the "no argument keeps working" rule is one testable function rather than a
// condition buried in a switch arm: an empty or whitespace-only argument is the
// same as no argument, and means the ambient pane.
func paneInfoRef(args []string) string {
	if len(args) < 2 {
		return ""
	}
	return strings.TrimSpace(args[1])
}
