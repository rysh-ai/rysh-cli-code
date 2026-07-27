package actors

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/upstream"
)

// extractShareTriFlag pulls an optional `--flag [true|false]` (or `--flag=value`)
// out of args, returning whether the flag was present, its boolean value (a bare
// flag ⇒ true; only a following true|false token is consumed), and the remaining
// args. Used for --share-api (default false ⇒ present&&value) and --redact
// (default true ⇒ present ? value : true).
func extractShareTriFlag(args []string, flag string) (present, value bool, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == flag:
			present, value = true, true
			if i+1 < len(args) && (args[i+1] == "true" || args[i+1] == "false") {
				value = args[i+1] == "true"
				i++
			}
		case strings.HasPrefix(a, flag+"="):
			present = true
			value = strings.TrimPrefix(a, flag+"=") != "false"
		default:
			rest = append(rest, a)
		}
	}
	return present, value, rest
}

// forgedOpsForEntity computes the forge-origin operation specs shareable for a
// given entity — the intersection of (ops the forge.Manager registered) ∩ (ops
// visible at the entity's scope chain). Built-ins / MCP / per-pane tools are
// excluded by construction (forge.Manager.SharedOps only sees forge ops).
func (w *WorkspaceActor) forgedOpsForEntity(entityType, entityID, capturePaneID string) []msg.ForgedOpSpec {
	if w.agSetup == nil || w.agSetup.Forge == nil || w.agSetup.Scopes == nil {
		return nil
	}
	keys := map[string]bool{"global": true}
	switch entityType {
	case "pane":
		ids := w.resolveScopeIDs(entityID)
		for _, k := range []string{
			agentic.ScopeKey(agentic.ScopeTab, ids),
			agentic.ScopeKey(agentic.ScopeLane, ids),
			agentic.ScopeKey(agentic.ScopeGroup, ids),
			agentic.ScopeKey(agentic.ScopePane, ids),
		} {
			if k != "" && k != "global" {
				keys[k] = true
			}
		}
	case "tab":
		keys["tab:"+entityID] = true
	case "lane":
		keys["lane:"+entityID] = true
	case "pane_group":
		keys["group:"+entityID] = true
	}
	ops := w.agSetup.Forge.SharedOps(keys)
	out := make([]msg.ForgedOpSpec, 0, len(ops))
	for _, o := range ops {
		out = append(out, msg.ForgedOpSpec{Name: o.Name, Description: o.Description, Schema: o.Schema, Mutating: o.Mutating})
	}
	return out
}

// autoShareFirstTab shares the workspace's first tab to the upstream workspace.
// It backs the global --shared flag and mirrors the behaviour of
// "##share tab [view|control]". The share mode follows [upstream]
// default_share_mode, defaulting to "control" so subscribers can drive the
// shared tab (set default_share_mode: view to override). When upstream is not
// enabled it leaves a note in the active pane instead of failing.
func (w *WorkspaceActor) autoShareFirstTab() {
	// Make sure activePaneID reflects the freshly bootstrapped first pane, so we
	// have somewhere to print status/errors.
	if w.activePaneID == "" {
		w.syncActivePane()
	}
	if !w.cfg.Upstream.Enabled || w.shareRegistryPID == nil {
		if w.activePaneID != "" {
			_ = w.pub.SendPaneRyshOutput(w.activePaneID,
				"\n[rysh] --shared was requested but upstream is not enabled;\n"+
					"  set upstream.enabled: true (with url/api_key/workspace) in rysh.config.yaml\n\n")
		}
		return
	}
	if len(w.tabs) == 0 {
		return
	}
	tab := w.tabs[0]
	if _, already := w.localShares[tab.id]; already {
		return
	}
	// Default to control unless the user explicitly configured view.
	mode := strings.ToLower(strings.TrimSpace(w.cfg.Upstream.DefaultShareMode))
	if mode != "view" {
		mode = "control"
	}
	shareID := w.beginShare("tab", tab.id, mode, tab.title, "", false, true)
	if w.activePaneID != "" {
		_ = w.pub.SendPaneRyshOutput(w.activePaneID, fmt.Sprintf(
			"\n[rysh] --shared: sharing first tab %q in %s mode (share: %s)\n"+
				"  to mirror from another session run:\n"+
				"  ##upstream subscribe %s %s\n\n", tab.title, mode, shareID, shareID, mode))
	}
}

// ---------------------------------------------------------------------------
// ##share / ##unshare / ##upstream command helpers
// ---------------------------------------------------------------------------

// showShareRestrictions fetches the pane's current restrictions via snapshot
// and formats them directly into out (so the mode-aware publisher handles display).
func (w *WorkspaceActor) showShareRestrictions(out *strings.Builder, paneID string) {
	res, err := w.pub.Request(msg.T("pane", paneID, "inbox"), &msg.MsgGetPaneSnapshot{}, time.Second)
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] failed to get pane snapshot: %v\n", err)
		return
	}
	reply, ok := res.(*msg.MsgPaneSnapshotReply)
	if !ok {
		fmt.Fprintf(out, "\n[rysh] unexpected reply type\n")
		return
	}

	paneLabel := paneID
	if len(paneLabel) > 8 {
		paneLabel = paneLabel[:8]
	}
	fmt.Fprintf(out, "\n[rysh] share restrictions for pane %s:\n", paneLabel)

	r := reply.Snapshot.ShareRestrictions
	if r == nil {
		out.WriteString("  disabled modes: none\n")
		out.WriteString("  shell: unrestricted\n")
		return
	}

	if len(r.DisabledModes) == 0 {
		out.WriteString("  disabled modes: none\n")
	} else {
		fmt.Fprintf(out, "  disabled modes: %s\n", strings.Join(r.DisabledModes, ", "))
	}

	switch {
	case len(r.ShellAllowList) > 0:
		fmt.Fprintf(out, "  shell: allow-list [%s]\n", strings.Join(r.ShellAllowList, ", "))
	case len(r.ShellForbidList) > 0:
		fmt.Fprintf(out, "  shell: forbid-list [%s]\n", strings.Join(r.ShellForbidList, ", "))
	default:
		out.WriteString("  shell: unrestricted\n")
	}
}

// normalizeCommandList splits each argument by commas and trims whitespace,
// so both "ls,cat" and "ls cat" (and "ls, cat") produce ["ls", "cat"].
func normalizeCommandList(args []string) []string {
	var result []string
	for _, arg := range args {
		for _, part := range strings.Split(arg, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				result = append(result, part)
			}
		}
	}
	return result
}

// handleShareCommand processes ##share subcommands.
// args are the parts after "share", e.g. ["pane", "view"] or ["list"] or ["status"].
func (w *WorkspaceActor) handleShareCommand(out *strings.Builder, paneID string, args []string) {
	if !w.cfg.Upstream.Enabled || w.shareRegistryPID == nil {
		fmt.Fprintf(out, "\n[rysh] upstream is not enabled\n")
		fmt.Fprintf(out, "  set upstream.enabled: true in rysh.config.yaml\n\n")
		return
	}

	if len(args) == 0 {
		w.shareHelp(out)
		return
	}

	sub := args[0]
	switch sub {
	case "pane":
		// Optional target: ##share pane --pane <id|name> ... shares a specific
		// pane instead of the active one.
		if pflag, rest := extractShareFlag(args, "--pane"); pflag != "" {
			resolved := w.resolvePaneID(pflag)
			if resolved == "" {
				fmt.Fprintf(out, "\n[rysh] no pane matching %q\n", pflag)
				return
			}
			paneID = resolved
			args = rest
		}
		if paneID == "" {
			fmt.Fprintf(out, "\n[rysh] no active pane\n")
			return
		}
		// File browsing is always enabled for a share. --allow-file-browse is
		// accepted for backward-compat but is a no-op. --allow-absolute widens
		// browsing beyond the pane's working-dir subtree (default: confined).
		// Parsed before the positional view/control arg so flags work in any order.
		_, args = extractShareBoolFlag(args, "--allow-file-browse")
		allowAbsolute, args := extractShareBoolFlag(args, "--allow-absolute")
		// Forged-API sharing is NOT part of ##share — it is a first-class forge
		// concept: share with `##forge share api <name>`, consume with
		// `##forge subscribe <name> [--scope ...]`. ##share only mirrors panes/tabs.
		// Check for restriction subcommands: disable, enable, shell, restrictions.
		if len(args) > 1 {
			switch args[1] {
			case "disable":
				// ##share pane disable mode <mode>
				if len(args) >= 4 && args[2] == "mode" {
					_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
						&msg.MsgShareDisableMode{PaneID: paneID, Mode: args[3]})
				} else {
					fmt.Fprintf(out, "\n[rysh] usage: ##share pane disable mode <sh|ai|rysh|chat>\n")
				}
				return
			case "enable":
				// ##share pane enable mode <mode>
				if len(args) >= 4 && args[2] == "mode" {
					_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
						&msg.MsgShareEnableMode{PaneID: paneID, Mode: args[3]})
				} else {
					fmt.Fprintf(out, "\n[rysh] usage: ##share pane enable mode <sh|ai|rysh|chat>\n")
				}
				return
			case "shell":
				// ##share pane shell allow|forbid|clear <commands...>
				if len(args) < 3 {
					fmt.Fprintf(out, "\n[rysh] usage: ##share pane shell allow|forbid|clear [commands...]\n")
					return
				}
				switch args[2] {
				case "allow":
					if len(args) < 4 {
						fmt.Fprintf(out, "\n[rysh] usage: ##share pane shell allow <cmd1> <cmd2> ...\n")
						return
					}
					cmds := normalizeCommandList(args[3:])
					_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
						&msg.MsgShareShellAllow{PaneID: paneID, Commands: cmds})
				case "forbid":
					if len(args) < 4 {
						fmt.Fprintf(out, "\n[rysh] usage: ##share pane shell forbid <cmd1> <cmd2> ...\n")
						return
					}
					cmds := normalizeCommandList(args[3:])
					_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
						&msg.MsgShareShellForbid{PaneID: paneID, Commands: cmds})
				case "clear":
					_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
						&msg.MsgShareShellClear{PaneID: paneID})
				default:
					fmt.Fprintf(out, "\n[rysh] usage: ##share pane shell allow|forbid|clear [commands...]\n")
				}
				return
			case "restrictions":
				// ##share pane restrictions — show restrictions inline.
				w.showShareRestrictions(out, paneID)
				return
			}
		}
		// Default: ##share pane [view|control]
		mode := "view"
		if len(args) > 1 && (args[1] == "view" || args[1] == "control") {
			mode = args[1]
		}
		shareID := w.beginShare("pane", paneID, mode, w.sharePaneAlias(paneID), paneID, false, true)
		// File browsing is always enabled (the responder is armed on connect).
		// If absolute browsing was requested, set it after the share starts; the
		// pane publishes updated restrictions, which UpstreamShareActor consumes.
		if allowAbsolute {
			_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgShareSetFileBrowse{
				PaneID:        paneID,
				AllowAbsolute: true,
			})
		}
		fmt.Fprintf(out, "\n[rysh] sharing pane %s in %s mode (share: %s)\n", paneID, mode, shareID)
		fmt.Fprintf(out, "  file browsing: enabled (allow-absolute: %v)\n", allowAbsolute)
		if root := w.shareRecords[paneID].SharedRootFolder; root != "" {
			fmt.Fprintf(out, "  browse root: %s%s\n", root, w.sharedRootNote())
		}
		fmt.Fprintf(out, "  to connect from another pane run:\n")
		fmt.Fprintf(out, "  ##upstream subscribe %s %s\n", shareID, mode)

	case "panegroup", "pg":
		if paneID == "" {
			fmt.Fprintf(out, "\n[rysh] no active pane\n")
			return
		}
		groupID := w.findActiveGroupID(paneID)
		if groupID == "" {
			fmt.Fprintf(out, "\n[rysh] could not determine active pane group\n")
			return
		}
		mode := "view"
		if len(args) > 1 && (args[1] == "view" || args[1] == "control") {
			mode = args[1]
		}
		groupAlias := fmt.Sprintf("group %.8s", groupID)
		if tab := w.currentTab(); tab != nil {
			groupAlias = fmt.Sprintf("%s · group", tab.title)
		}
		shareID := w.beginShare("pane_group", groupID, mode, groupAlias, paneID, false, true)
		fmt.Fprintf(out, "\n[rysh] sharing pane group %.8s in %s mode (share: %s)\n", groupID, mode, shareID)
		fmt.Fprintf(out, "  to mirror from another session run:\n")
		fmt.Fprintf(out, "  ##upstream subscribe %s %s\n", shareID, mode)

	case "lane":
		if paneID == "" {
			fmt.Fprintf(out, "\n[rysh] no active pane\n")
			return
		}
		laneID := w.findActiveLaneID(paneID)
		if laneID == "" {
			fmt.Fprintf(out, "\n[rysh] could not determine active lane\n")
			return
		}
		mode := "view"
		if len(args) > 1 && (args[1] == "view" || args[1] == "control") {
			mode = args[1]
		}
		laneAlias := fmt.Sprintf("lane %.8s", laneID)
		if tab := w.currentTab(); tab != nil {
			laneAlias = fmt.Sprintf("%s · lane", tab.title)
		}
		shareID := w.beginShare("lane", laneID, mode, laneAlias, paneID, false, true)
		fmt.Fprintf(out, "\n[rysh] sharing lane %.8s in %s mode (share: %s)\n", laneID, mode, shareID)
		fmt.Fprintf(out, "  to mirror from another session run:\n")
		fmt.Fprintf(out, "  ##upstream subscribe %s %s\n", shareID, mode)

	case "tab":
		// Optional target: ##share tab --tab <id|index|title> ... shares a
		// specific tab instead of the active one.
		tab := w.currentTab()
		if tflag, rest := extractShareFlag(args, "--tab"); tflag != "" {
			tab = w.resolveTabArg(tflag)
			if tab == nil {
				fmt.Fprintf(out, "\n[rysh] no tab matching %q\n", tflag)
				return
			}
			args = rest
		}
		if tab == nil {
			fmt.Fprintf(out, "\n[rysh] no active tab\n")
			return
		}
		mode := "view"
		if len(args) > 1 && (args[1] == "view" || args[1] == "control") {
			mode = args[1]
		}
		shareID := w.beginShare("tab", tab.id, mode, tab.title, paneID, false, true)
		fmt.Fprintf(out, "\n[rysh] sharing tab %q in %s mode (share: %s)\n", tab.title, mode, shareID)
		if root := w.shareRecords[tab.id].SharedRootFolder; root != "" {
			fmt.Fprintf(out, "  browse root (all panes): %s%s\n", root, w.sharedRootNote())
		}
		fmt.Fprintf(out, "  to mirror from another session run:\n")
		fmt.Fprintf(out, "  ##upstream subscribe %s %s\n", shareID, mode)

	case "list":
		res, err := w.system().Root.RequestFuture(w.shareRegistryPID, &msg.MsgShareList{}, time.Second).Result()
		if err != nil {
			fmt.Fprintf(out, "\n[rysh] failed to get share list: %v\n", err)
			return
		}
		lr, ok := res.(*msg.MsgShareListReply)
		if !ok {
			fmt.Fprintf(out, "\n[rysh] unexpected reply type\n")
			return
		}
		if len(lr.Shares) == 0 {
			fmt.Fprintf(out, "\n[rysh] no active shares\n")
			return
		}
		fmt.Fprintf(out, "\n[rysh] active shares (%d)\n", len(lr.Shares))
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 80))
		for _, s := range lr.Shares {
			connLabel := "connected"
			if !s.Connected {
				connLabel = "disconnected"
			}
			fmt.Fprintf(out, "  %-8s  share=%s\n", s.EntityType, s.ShareID)
			fmt.Fprintf(out, "           entity=%.8s  mode=%-7s  %s\n",
				s.EntityID, s.Mode, connLabel)
		}
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 80))

	case "status":
		entityID := ""
		if paneID != "" {
			entityID = paneID
		}
		res, err := w.system().Root.RequestFuture(w.shareRegistryPID, &msg.MsgShareStatus{EntityID: entityID}, time.Second).Result()
		if err != nil {
			fmt.Fprintf(out, "\n[rysh] failed to get share status: %v\n", err)
			return
		}
		sr, ok := res.(*msg.MsgShareStatusReply)
		if !ok {
			fmt.Fprintf(out, "\n[rysh] unexpected reply type\n")
			return
		}
		if len(sr.Shares) == 0 {
			fmt.Fprintf(out, "\n[rysh] no shares found for this entity\n")
			return
		}
		fmt.Fprintf(out, "\n[rysh] share status\n")
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
		for _, s := range sr.Shares {
			connLabel := "connected"
			if !s.Connected {
				connLabel = "disconnected"
			}
			fmt.Fprintf(out, "  share-id   : %s\n", s.ShareID)
			fmt.Fprintf(out, "  type       : %s\n", s.EntityType)
			fmt.Fprintf(out, "  entity     : %s\n", s.EntityID)
			fmt.Fprintf(out, "  mode       : %s\n", s.Mode)
			fmt.Fprintf(out, "  status     : %s\n", connLabel)
			fmt.Fprintf(out, "  url        : %s\n", s.URL)
			fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
		}

	default:
		w.shareHelp(out)
	}
}

// handleUnshareCommand processes ##unshare subcommands.
func (w *WorkspaceActor) handleUnshareCommand(out *strings.Builder, paneID string, args []string) {
	if !w.cfg.Upstream.Enabled || w.shareRegistryPID == nil {
		fmt.Fprintf(out, "\n[rysh] upstream is not enabled\n")
		fmt.Fprintf(out, "  set upstream.enabled: true in rysh.config.yaml\n\n")
		return
	}

	if len(args) == 0 {
		fmt.Fprintf(out, "\n[rysh] usage: ##unshare pane|panegroup|lane|tab\n")
		return
	}

	sub := args[0]
	switch sub {
	case "pane":
		if paneID == "" {
			fmt.Fprintf(out, "\n[rysh] no active pane\n")
			return
		}
		w.endShare(paneID)
		fmt.Fprintf(out, "\n[rysh] unshared pane %s\n", paneID[:8])

	case "panegroup", "pg":
		if paneID == "" {
			fmt.Fprintf(out, "\n[rysh] no active pane\n")
			return
		}
		groupID := w.findActiveGroupID(paneID)
		if groupID == "" {
			fmt.Fprintf(out, "\n[rysh] could not determine active pane group\n")
			return
		}
		w.endShare(groupID)
		fmt.Fprintf(out, "\n[rysh] unshared pane group %.8s\n", groupID)

	case "lane":
		if paneID == "" {
			fmt.Fprintf(out, "\n[rysh] no active pane\n")
			return
		}
		laneID := w.findActiveLaneID(paneID)
		if laneID == "" {
			fmt.Fprintf(out, "\n[rysh] could not determine active lane\n")
			return
		}
		w.endShare(laneID)
		fmt.Fprintf(out, "\n[rysh] unshared lane %.8s\n", laneID)

	case "tab":
		tab := w.currentTab()
		if tab == nil {
			fmt.Fprintf(out, "\n[rysh] no active tab\n")
			return
		}
		w.endShare(tab.id)
		fmt.Fprintf(out, "\n[rysh] unshared tab %q\n", tab.title)

	default:
		fmt.Fprintf(out, "\n[rysh] usage: ##unshare pane|panegroup|lane|tab\n")
	}
}

// beginShare records a share intent so it survives a session restart, then
// starts it via the registry. If the entity was shared before (e.g. restored
// from KV), the original shareID is reused so existing subscribers reconnect
// instead of needing a fresh id. Returns the shareID for the caller to display.
func (w *WorkspaceActor) beginShare(entityType, entityID, mode, alias, capturePaneID string, shareAPI, redact bool) string {
	shareID := ""
	if rec, ok := w.shareRecords[entityID]; ok {
		shareID = rec.ShareID
	}
	if shareID == "" {
		shareID = uuid.NewString()
	}
	// Capture the CURRENT working directory as the file-browse root every time an
	// explicit ##share runs, so it reflects the user's present directory instead
	// of a stale value pinned by an earlier share. For a tab/lane/group share this
	// is the submitting pane's working directory, applied to every pane in the
	// entity. Restart re-establishment reuses the persisted value (see
	// reshareActiveShares), so this does not re-run on restart.
	sharedRoot, src := w.captureSharedRoot(entityType, entityID, capturePaneID)
	w.lastSharedRootSource = src
	w.localShares[entityID] = shareID
	w.shareRecords[entityID] = shareKV{
		ShareID:          shareID,
		EntityType:       entityType,
		EntityID:         entityID,
		Mode:             mode,
		EntityAlias:      alias,
		SharedRootFolder: sharedRoot,
		ShareAPI:         shareAPI,
		Redact:           redact,
	}
	var forgedOps []msg.ForgedOpSpec
	if shareAPI {
		forgedOps = w.forgedOpsForEntity(entityType, entityID, capturePaneID)
	}
	w.system().Root.Send(w.shareRegistryPID, &msg.MsgShareEntity{
		EntityType:       entityType,
		EntityID:         entityID,
		Mode:             mode,
		ShareID:          shareID,
		EntityAlias:      alias,
		SharedRootFolder: sharedRoot,
		ShareAPI:         shareAPI,
		Redact:           redact,
		ForgedOps:        forgedOps,
	})
	w.persistToKV()
	return shareID
}

// captureSharedRoot resolves the working directory to pin as the file-browse
// root for a new share, and reports how it was resolved ("live"/"startup"/
// "daemon"/""). For a pane share the pane is the entity; for a tab/lane/group
// share it uses capturePaneID (the pane the user typed ##share in), falling back
// to the globally-active pane. Returns ("","") when no pane is resolvable, in
// which case the browse root falls back to live per-request resolution.
func (w *WorkspaceActor) captureSharedRoot(entityType, entityID, capturePaneID string) (root, source string) {
	paneID := entityID
	if entityType != "pane" {
		paneID = capturePaneID
		if paneID == "" {
			paneID = w.activePaneID
		}
	}
	if paneID == "" {
		return "", ""
	}
	res, err := w.pub.Request(msg.T("pane", paneID, "inbox"), &msg.MsgGetPaneSnapshot{}, time.Second)
	if err != nil {
		return "", ""
	}
	reply, ok := res.(*msg.MsgPaneSnapshotReply)
	if !ok {
		return "", ""
	}
	root, source = resolvePaneRootWithSource(reply.Snapshot.ShellPID, reply.Snapshot.StartupDir)
	slog.Info("share: captured browse root",
		"entityType", entityType, "pane", paneID,
		"shellPID", reply.Snapshot.ShellPID, "root", root, "source", source)
	return root, source
}

// sharedRootNote returns a human-readable suffix describing how the last
// captured browse root was resolved, so the ##share confirmation tells the user
// whether it read the pane's live directory or fell back. Empty for "live".
func (w *WorkspaceActor) sharedRootNote() string {
	switch w.lastSharedRootSource {
	case "startup":
		return " (pane startup dir)"
	case "daemon":
		return " (rysh launch dir — could not read the pane's live cwd; cd into your project in the pane, then re-share)"
	default: // "live" or unknown: no note
		return ""
	}
}

// endShare stops a share and forgets its persisted intent.
func (w *WorkspaceActor) endShare(entityID string) {
	w.system().Root.Send(w.shareRegistryPID, &msg.MsgUnshareEntity{EntityID: entityID})
	delete(w.localShares, entityID)
	delete(w.shareRecords, entityID)
	w.persistToKV()
}

// shareList returns the persisted share intents for KV serialisation.
func (w *WorkspaceActor) shareList() []shareKV {
	if len(w.shareRecords) == 0 {
		return nil
	}
	out := make([]shareKV, 0, len(w.shareRecords))
	for _, rec := range w.shareRecords {
		out = append(out, rec)
	}
	return out
}

// hasTab reports whether a tab with the given id currently exists.
func (w *WorkspaceActor) hasTab(id string) bool {
	for _, t := range w.tabs {
		if t.id == id {
			return true
		}
	}
	return false
}

// reshareActiveShares re-establishes shares that were active before a restart.
// The local publisher (UpstreamShareActor) does not survive a restart, but the
// upstream server keeps the share record — so without this a restored session
// leaves its shares as "ghosts": the share still lists upstream and subscribers
// can subscribe, yet no layout is ever published and they hang on
// "waiting for layout". Each persisted share is re-issued to the registry
// REUSING the original shareID, so existing subscribers resume transparently.
func (w *WorkspaceActor) reshareActiveShares() {
	if w.shareRegistryPID == nil || len(w.shareRecords) == 0 {
		return
	}
	for entityID, rec := range w.shareRecords {
		// Tab existence is cheap to verify (w.tabs is populated synchronously by
		// restore). Pane/lane/group entities live in child actors that may not be
		// initialised yet here, so they are re-established optimistically — the
		// UpstreamShareActor publishes a closed layout if the entity is gone.
		if rec.EntityType == "tab" && !w.hasTab(entityID) {
			continue
		}
		w.localShares[entityID] = rec.ShareID
		// Recompute the forge-origin op specs from the live forge manager rather
		// than persisting them, so a changed integration set (ops added/removed
		// since the share was created) is reflected after restart.
		var forgedOps []msg.ForgedOpSpec
		if rec.ShareAPI {
			forgedOps = w.forgedOpsForEntity(rec.EntityType, entityID, "")
		}
		w.system().Root.Send(w.shareRegistryPID, &msg.MsgShareEntity{
			EntityType:       rec.EntityType,
			EntityID:         entityID,
			Mode:             rec.Mode,
			ShareID:          rec.ShareID,
			EntityAlias:      rec.EntityAlias,
			SharedRootFolder: rec.SharedRootFolder,
			ShareAPI:         rec.ShareAPI,
			Redact:           rec.Redact,
			ForgedOps:        forgedOps,
		})
		slog.Info("reshare: re-established share after restart",
			"shareID", rec.ShareID, "type", rec.EntityType,
			"entityID", entityID, "mode", rec.Mode, "shareAPI", rec.ShareAPI, "forgedOps", len(forgedOps))
	}
}

// handleUpstreamShares lists shares published by this session to the upstream.
func (w *WorkspaceActor) handleUpstreamShares(out *strings.Builder) {
	if !w.cfg.Upstream.Enabled || w.shareRegistryPID == nil {
		fmt.Fprintf(out, "\n[rysh] upstream is not enabled\n")
		return
	}
	res, err := w.system().Root.RequestFuture(w.shareRegistryPID, &msg.MsgShareList{}, time.Second).Result()
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] failed to query shares: %v\n", err)
		return
	}
	lr, ok := res.(*msg.MsgShareListReply)
	if !ok {
		fmt.Fprintf(out, "\n[rysh] unexpected reply type\n")
		return
	}
	if len(lr.Shares) == 0 {
		fmt.Fprintf(out, "\n[rysh] no shares published from this session\n")
		return
	}
	fmt.Fprintf(out, "\n[rysh] shares published from this session (%d)\n", len(lr.Shares))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 80))
	for _, s := range lr.Shares {
		fmt.Fprintf(out, "  %-8s  share=%s\n", s.EntityType, s.ShareID)
		fmt.Fprintf(out, "           entity=%.8s  mode=%-7s\n",
			s.EntityID, s.Mode)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 80))
}

// handleUpstreamListRemote fetches all shares in the workspace from the upstream server.
func (w *WorkspaceActor) handleUpstreamListRemote(out *strings.Builder) {
	if !w.cfg.Upstream.Enabled {
		fmt.Fprintf(out, "\n[rysh] upstream is not enabled\n")
		return
	}

	shares, err := upstream.FetchRemoteShares(w.cfg)
	if err != nil {
		fmt.Fprintf(out, "\n[rysh] failed to fetch remote shares: %v\n", err)
		return
	}
	if len(shares) == 0 {
		fmt.Fprintf(out, "\n[rysh] no shares found in the workspace\n")
		return
	}

	fmt.Fprintf(out, "\n[rysh] remote shares in workspace (%d)\n", len(shares))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 80))
	for _, s := range shares {
		alias := s.EntityAlias
		if alias == "" {
			alias = s.EntityID[:8]
		}
		fmt.Fprintf(out, "  %-8s  share=%s\n", s.EntityType, s.ShareID)
		fmt.Fprintf(out, "           alias=%-20s  mode=%-7s  origin=%-8s  state=%s\n",
			alias, s.ShareMode, s.Origin, s.State)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 80))
	fmt.Fprintf(out, "  Use: ##upstream subscribe <shareID> [view|control]\n")
}

// handleUpstreamSubscribe spawns a RemoteShareListenerActor for the given share.
func (w *WorkspaceActor) handleUpstreamSubscribe(out *strings.Builder, paneID, shareID, mode string) {
	if !w.cfg.Upstream.Enabled {
		fmt.Fprintf(out, "\n[rysh] upstream is not enabled\n")
		return
	}
	if shareID == "" {
		fmt.Fprintf(out, "\n[rysh] usage: ##upstream subscribe <shareID> [view|control]\n")
		return
	}
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane to receive share output\n")
		return
	}

	// Resolve the given ID against the local share registry.
	// Accepts a share ID, an entity ID (pane ID), or a prefix of either.
	if w.shareRegistryPID != nil {
		resolved, debugInfo := w.resolveToShareID(shareID)
		if resolved != "" {
			if resolved != shareID {
				fmt.Fprintf(out, "\n[rysh] resolved %.8s → share %s\n", shareID, resolved)
			}
			shareID = resolved
		} else {
			fmt.Fprintf(out, "\n[rysh] warn: could not resolve ID %.8s (%s)\n", shareID, debugInfo)
		}
	} else {
		fmt.Fprintf(out, "\n[rysh] warn: share registry not available\n")
	}

	// Default to "view" if mode is empty or invalid.
	switch mode {
	case "view", "control":
		// valid
	default:
		mode = "view"
	}

	// Look up the share metadata from the server: the origin (to route chatbot
	// output to the external buffer) and the entity type (a tab/lane/pane_group
	// share is rendered as a mirror tab rather than streamed into a single pane).
	origin := "terminal"
	entityType := ""
	entityAlias := ""
	if remoteShares, err := upstream.FetchRemoteShares(w.cfg); err == nil {
		for _, rs := range remoteShares {
			if rs.ShareID == shareID {
				origin = rs.Origin
				entityType = rs.EntityType
				entityAlias = rs.EntityAlias
				break
			}
		}
	}

	// Tab / lane / pane_group shares become a mirror tab (view or control).
	if isMirrorEntityType(entityType) {
		w.subscribeMirrorTab(out, shareID, entityType, entityAlias, mode)
		return
	}

	// Stop existing listener if any, and clear connection/controller state on the old pane.
	if w.remoteListenerPID != nil {
		if w.remoteListenerPaneID != "" {
			_ = w.pub.Send(
				msg.T("pane", w.remoteListenerPaneID, "inbox"),
				&msg.MsgSetConnectedPane{PaneID: ""},
			)
		}
		if w.remoteListenerMode == "control" && w.remoteListenerPaneID != "" {
			_ = w.pub.Send(
				msg.T("pane", w.remoteListenerPaneID, "inbox"),
				&msg.MsgSetControllerMode{Active: false},
			)
		}
		w.system().Root.Stop(w.remoteListenerPID)
		w.remoteListenerPID = nil
		w.remoteListenerPaneID = ""
		w.remoteListenerMode = ""
		w.remoteListenerShareID = ""
	}

	props := actor.PropsFromProducer(func() actor.Actor {
		return NewRemoteShareListenerActor(shareID, paneID, mode, origin, w.sessionName, w.cfg.Profile.Name, w.cfg.Upstream, w.pub, w.nc)
	})
	w.remoteListenerPID = w.system().Root.Spawn(props)
	w.remoteListenerPaneID = paneID
	w.remoteListenerMode = mode
	w.remoteListenerShareID = shareID
	fmt.Fprintf(out, "\n[rysh] subscribed to share %s\n", shareID)
	fmt.Fprintf(out, "  mode: %s | origin: %s | output → pane %.8s\n", mode, origin, paneID)

	// For chatbot shares, seed the external output buffer so the "external"
	// mode becomes available in the mode cycle, and auto-switch to it.
	if origin == "chatbot" {
		_ = w.pub.SendPaneExternalOutput(paneID,
			fmt.Sprintf("[chatbot] connected to remote share %s\n", shareID[:8]))
	}

	// Resolve the entity (remote pane) ID from the share registry and notify
	// the subscribing pane so it can display the connection in its status line.
	if remotePaneID := w.resolveShareEntityID(shareID); remotePaneID != "" {
		_ = w.pub.Send(
			msg.T("pane", paneID, "inbox"),
			&msg.MsgSetConnectedPane{PaneID: remotePaneID},
		)
	}

	// In controller mode, notify the pane so it forwards shell/prompt/chat input.
	if mode == "control" {
		// Use the first 8 chars of the shareID as the display alias.
		alias := shareID
		if len(alias) > 8 {
			alias = alias[:8]
		}
		_ = w.pub.Send(
			msg.T("pane", paneID, "inbox"),
			&msg.MsgSetControllerMode{
				ShareID:   shareID,
				PaneAlias: alias,
				Active:    true,
			},
		)
	}
}

// resolveToShareID resolves the given ID to a share ID. Checks the workspace's
// local share map first (always available), then falls back to the share registry.
func (w *WorkspaceActor) resolveToShareID(id string) (string, string) {
	slog.Info("resolveToShareID",
		"id", id, "localSharesSize", len(w.localShares))
	for k, v := range w.localShares {
		slog.Info("resolveToShareID: localShares entry",
			"entityID", k, "shareID", v)
	}

	// 1. Check local share map (paneID → shareID).
	if shareID, ok := w.localShares[id]; ok {
		slog.Info("resolveToShareID: exact match in localShares",
			"id", id, "shareID", shareID)
		return shareID, ""
	}
	// Check if id is already a known shareID (reverse lookup).
	for _, shareID := range w.localShares {
		if shareID == id {
			slog.Info("resolveToShareID: reverse match in localShares",
				"id", id, "shareID", shareID)
			return shareID, ""
		}
	}
	// Prefix match on local map.
	for entityID, shareID := range w.localShares {
		if strings.HasPrefix(entityID, id) || strings.HasPrefix(shareID, id) {
			slog.Info("resolveToShareID: prefix match in localShares",
				"id", id, "entityID", entityID, "shareID", shareID)
			return shareID, ""
		}
	}

	// 2. Fall back to share registry actor request.
	res, err := w.system().Root.RequestFuture(w.shareRegistryPID, &msg.MsgShareList{}, time.Second).Result()
	if err != nil {
		return "", fmt.Sprintf("registry request failed: %v", err)
	}
	lr, ok := res.(*msg.MsgShareListReply)
	if !ok {
		return "", fmt.Sprintf("unexpected reply type: %T", res)
	}
	if len(lr.Shares) == 0 {
		// If the ID looks like a valid UUID, accept it directly.
		// This handles cross-session scenarios where this session has
		// no local shares but the ID is a valid share reference from
		// another session (reachable via the upstream server).
		if _, err := uuid.Parse(id); err == nil {
			slog.Info("resolveToShareID: registry empty but ID is valid UUID, using directly",
				"id", id)
			return id, ""
		}
		return "", "no shares registered"
	}

	// Build debug info showing what the registry has.
	var ids []string
	for _, s := range lr.Shares {
		ids = append(ids, fmt.Sprintf("share=%.8s entity=%.8s", s.ShareID, s.EntityID))
	}
	debugShares := strings.Join(ids, "; ")

	// 1. Exact match on share ID.
	for _, s := range lr.Shares {
		if s.ShareID == id {
			return s.ShareID, ""
		}
	}
	// 2. Exact match on entity ID (pane ID).
	for _, s := range lr.Shares {
		if s.EntityID == id {
			return s.ShareID, ""
		}
	}
	// 3. Prefix match on share ID.
	var match string
	for _, s := range lr.Shares {
		if strings.HasPrefix(s.ShareID, id) {
			if match != "" {
				return "", "ambiguous share prefix"
			}
			match = s.ShareID
		}
	}
	if match != "" {
		return match, ""
	}
	// 4. Prefix match on entity ID.
	for _, s := range lr.Shares {
		if strings.HasPrefix(s.EntityID, id) {
			if match != "" {
				return "", "ambiguous entity prefix"
			}
			match = s.ShareID
		}
	}
	if match != "" {
		return match, ""
	}
	// Final fallback: if the ID is a valid UUID, use it directly.
	// The registry may not have this share (e.g. cross-session sharing).
	if _, err := uuid.Parse(id); err == nil {
		slog.Info("resolveToShareID: no registry match but ID is valid UUID, using directly",
			"id", id)
		return id, ""
	}
	return "", fmt.Sprintf("no match for %.8s in registry [%s]", id, debugShares)
}

// resolveShareEntityID returns the entity ID (pane ID) for the given shareID.
// It checks the workspace's local share map first, then falls back to the
// share registry actor.
func (w *WorkspaceActor) resolveShareEntityID(shareID string) string {
	// 1. Reverse-lookup in localShares (entityID → shareID).
	for entityID, sid := range w.localShares {
		if sid == shareID {
			return entityID
		}
	}

	// 2. Fall back to share registry actor request.
	if w.shareRegistryPID == nil {
		return ""
	}
	res, err := w.system().Root.RequestFuture(w.shareRegistryPID, &msg.MsgShareList{}, time.Second).Result()
	if err != nil {
		return ""
	}
	lr, ok := res.(*msg.MsgShareListReply)
	if !ok {
		return ""
	}
	for _, s := range lr.Shares {
		if s.ShareID == shareID {
			return s.EntityID
		}
	}
	return ""
}

// subscribeMirrorTab creates a mirror tab for a remote tab/lane/pane_group
// share and spawns a MirrorTabListenerActor to feed it. The new tab is appended
// after the real tabs and becomes the active selection. In control mode the
// subscriber can run commands on the source panes; in view mode it is read-only.
func (w *WorkspaceActor) subscribeMirrorTab(out *strings.Builder, shareID, entityType, alias, mode string) {
	if mode != "control" {
		mode = "view"
	}
	if mt := w.mirrorTabByShareID(shareID); mt != nil {
		// Already mirrored — just focus it.
		for i, m2 := range w.mirrorTabs {
			if m2 == mt {
				w.activeTabIdx = len(w.tabs) + i
				break
			}
		}
		w.syncActivePane()
		fmt.Fprintf(out, "\n[rysh] already mirroring share %.8s — focused its tab\n", shareID)
		return
	}

	if alias == "" {
		alias = shareID
		if len(alias) > 8 {
			alias = alias[:8]
		}
	}

	mt := w.addMirrorTab(shareID, alias)
	mt.mode = mode
	props := actor.PropsFromProducer(func() actor.Actor {
		return NewMirrorTabListenerActor(shareID, alias, mode, w.cfg.Profile.Name, w.cfg.Upstream, w.pub, w.selfPID)
	})
	mt.listenerPID = w.system().Root.Spawn(props)

	// Focus the new mirror tab.
	w.activeTabIdx = w.tabCount() - 1
	w.syncActivePane()

	fmt.Fprintf(out, "\n[rysh] mirroring remote %s %q in %s mode (share %.8s)\n", entityType, alias, mode, shareID)
	if mode == "control" {
		fmt.Fprintf(out, "  control mode: focus a pane (Tab/click) and type to run commands on the source pane\n")
		fmt.Fprintf(out, "  (requires the owner to have shared with control); close with Ctrl+P x\n")
	} else {
		fmt.Fprintf(out, "  read-only mirror — close it (Ctrl+P x) or ##upstream unsubscribe to stop\n")
	}
}

// handleUpstreamUnsubscribe stops the active RemoteShareListenerActor, or — when
// arg names/identifies a mirror tab (or one is the active selection) — removes
// that mirror tab instead.
func (w *WorkspaceActor) handleUpstreamUnsubscribe(out *strings.Builder, arg string) {
	// Targeted mirror-tab removal by share ID / prefix.
	if arg != "" {
		for _, mt := range w.mirrorTabs {
			if mt.shareID == arg || strings.HasPrefix(mt.shareID, arg) || mt.alias == arg {
				w.removeMirrorTab(mt.shareID)
				w.persistToKV()
				fmt.Fprintf(out, "\n[rysh] stopped mirroring share %.8s\n", mt.shareID)
				return
			}
		}
	}
	// If a mirror tab is the active selection, remove it.
	if mt := w.activeMirrorTab(); mt != nil {
		w.removeMirrorTab(mt.shareID)
		w.persistToKV()
		fmt.Fprintf(out, "\n[rysh] stopped mirroring share %.8s\n", mt.shareID)
		return
	}
	// No pane subscription and no targeted/active mirror: if exactly one mirror
	// tab exists, remove it; otherwise report which mirrors are open.
	if w.remoteListenerPID == nil {
		switch len(w.mirrorTabs) {
		case 0:
			fmt.Fprintf(out, "\n[rysh] no active remote subscription\n")
		case 1:
			sid := w.mirrorTabs[0].shareID
			w.removeMirrorTab(sid)
			w.persistToKV()
			fmt.Fprintf(out, "\n[rysh] stopped mirroring share %.8s\n", sid)
		default:
			fmt.Fprintf(out, "\n[rysh] multiple mirror tabs open; specify one:\n")
			for _, mt := range w.mirrorTabs {
				fmt.Fprintf(out, "  ##upstream unsubscribe %.8s   (%s)\n", mt.shareID, mt.alias)
			}
		}
		return
	}
	// Clear connected-to state on the pane.
	if w.remoteListenerPaneID != "" {
		_ = w.pub.Send(
			msg.T("pane", w.remoteListenerPaneID, "inbox"),
			&msg.MsgSetConnectedPane{PaneID: ""},
		)
	}
	// Clear controller mode on the pane if it was in control mode.
	if w.remoteListenerMode == "control" && w.remoteListenerPaneID != "" {
		_ = w.pub.Send(
			msg.T("pane", w.remoteListenerPaneID, "inbox"),
			&msg.MsgSetControllerMode{Active: false},
		)
	}
	w.system().Root.Stop(w.remoteListenerPID)
	w.remoteListenerPID = nil
	w.remoteListenerPaneID = ""
	w.remoteListenerMode = ""
	w.remoteListenerShareID = ""
	fmt.Fprintf(out, "\n[rysh] unsubscribed from remote share\n")
}

// handleUpstreamSend forwards a command to the active remote share listener.
func (w *WorkspaceActor) handleUpstreamSend(out *strings.Builder, text string) {
	if w.remoteListenerPID == nil {
		fmt.Fprintf(out, "\n[rysh] no active remote subscription\n")
		return
	}
	if text == "" {
		fmt.Fprintf(out, "\n[rysh] usage: ##upstream send <text>\n")
		return
	}
	w.system().Root.Send(w.remoteListenerPID, &msg.MsgUpstreamSendCommand{
		CommandType: "submit_input",
		Payload:     text,
	})
	fmt.Fprintf(out, "\n[rysh] sent command to remote share\n")
}

// shareHelp prints usage for ##share.
func (w *WorkspaceActor) shareHelp(out *strings.Builder) {
	fmt.Fprintf(out, "\n[rysh] usage:\n")
	fmt.Fprintf(out, "  ##share pane [--pane <id|name>] [view|control] [--allow-absolute]   share a pane (default: active; file browsing always enabled, --allow-absolute widens beyond the working dir)\n")
	fmt.Fprintf(out, "  ##share panegroup [view|control]   share the active pane group\n")
	fmt.Fprintf(out, "  ##share lane [view|control]        share the active lane\n")
	fmt.Fprintf(out, "  ##share tab [--tab <id|index|title>] [view|control]  share a tab (default: active)\n")
	fmt.Fprintf(out, "  ##share list                       list all active shares\n")
	fmt.Fprintf(out, "  ##share status                     show share status for active pane\n")
	fmt.Fprintf(out, "\n  sharing a forged API is separate from ##share — use:\n")
	fmt.Fprintf(out, "    ##forge share api <name>                          (source) expose an enabled API\n")
	fmt.Fprintf(out, "    ##forge list-remote                               (subscriber) list shared APIs\n")
	fmt.Fprintf(out, "    ##forge subscribe <name> [--scope pane|panegroup|lane|tab]   (subscriber) mount it\n\n")
}

// findActiveGroupID returns the pane group ID containing the given pane.
// sharePaneAlias builds the human-readable alias registered with the upstream
// when sharing a pane, so remote viewers (e.g. the mobile shares list) can tell
// what each shared pane is about. It packs the user-given name and the
// auto-assigned title as "<given> · <auto>", or just "<auto>" when there is no
// distinct given name. Falls back to a short pane id when no title is set.
func (w *WorkspaceActor) sharePaneAlias(paneID string) string {
	auto, given := "", ""
	if tab := w.currentTab(); tab != nil {
		if tabSnap := w.queryTabSnapshot(tab.id); tabSnap != nil {
			for _, lane := range tabSnap.Lanes {
				for _, g := range lane.PaneGroups {
					for i := range g.Panes {
						if g.Panes[i].ID == paneID {
							auto = strings.TrimSpace(g.Panes[i].Title)
							given = strings.TrimSpace(g.Panes[i].GivenName)
						}
					}
				}
			}
		}
	}
	if auto == "" {
		if len(paneID) >= 8 {
			auto = paneID[:8]
		} else {
			auto = paneID
		}
	}
	if given != "" && given != auto {
		return given + " · " + auto
	}
	return auto
}

// resolveTabArg resolves a tab from a user-supplied argument. The argument may
// be a tab ID, a tab title, or a 1-based index (as shown by ##tab list). An
// empty argument resolves to the active tab. Returns nil if no match.
// extractShareFlag removes the first occurrence of flag and its value from args,
// returning the value and the remaining args. If flag is absent it returns ""
// and args unchanged. Used to parse ##share targeting flags (--tab / --pane)
// without disturbing the rest of the argument parsing.
func extractShareFlag(args []string, flag string) (string, []string) {
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			value := ""
			if i+1 < len(args) {
				value = args[i+1]
			}
			rest := make([]string, 0, len(args))
			rest = append(rest, args[:i]...)
			if i+2 <= len(args) {
				rest = append(rest, args[i+2:]...)
			}
			return value, rest
		}
	}
	return "", args
}

// extractShareBoolFlag removes the first occurrence of a valueless boolean flag
// (e.g. --allow-file-browse) from args, returning whether it was present and the
// remaining args. Used by ##share to parse capability toggles without disturbing
// the positional view/control argument.
func extractShareBoolFlag(args []string, flag string) (bool, []string) {
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			rest := make([]string, 0, len(args))
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return true, rest
		}
	}
	return false, args
}
