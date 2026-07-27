package actors

import (
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/forgecmd"
)

// handleForgeSubcommand implements the in-session `##forge` command — the
// daemon-side mirror of the `rysh forge` CLI for the artifact subcommands, plus
// the runtime forged-API SHARING subcommands (handled by the ForgeShareActor).
//
// Artifact subcommands (delegated to internal/forge/forgecmd):
//
//	##forge add <name> <spec-file> [flags]   ingest a spec + generate artifacts
//	##forge generate <name> [flags]          re-generate from the stored spec
//	##forge list                             list configured integrations
//	##forge diff <name> <new-spec-file>      operation-level changes vs the stored spec
//	##forge targets                          list available generator targets
//
// After `##forge add`, make the tools live with `##integration enable <name>`.
//
// Forged-API sharing subcommands (peer-to-peer over the upstream workspace):
//
//	##forge share api <name>                 (source) expose an enabled API to subscribers
//	##forge unshare api <name>               (source) stop exposing it
//	##forge shares                           (source) list the APIs THIS session is sharing
//	##forge list-remote                      (subscriber) list APIs shared in the workspace
//	##forge subscribe <name> [--scope ...]   (subscriber) mount it as tools at the chosen scope
//	##forge unsubscribe <name>               (subscriber) remove it
//	##forge subscriptions                    (subscriber) list THIS session's active subscriptions
func (w *WorkspaceActor) handleForgeSubcommand(out *strings.Builder, paneID string, args []string) {
	if w.agSetup == nil || w.agSetup.Forge == nil {
		fmt.Fprintf(out, "\n[forge] Forge is unavailable (agentic mode disabled?)\n")
		return
	}

	// Runtime forged-API sharing subcommands route to the ForgeShareActor; results
	// stream to the pane's rysh output (discovery/invocation involve the network).
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "share":
			w.handleForgeShare(out, paneID, args[1:])
			return
		case "unshare":
			w.handleForgeUnshare(out, paneID, args[1:])
			return
		case "list-remote", "remote", "ls-remote":
			w.forgeShareDispatch(out, paneID, &msgForgeListRemote{PaneID: paneID}, "querying remote shared APIs…")
			return
		case "shares", "shared":
			w.forgeShareDispatch(out, paneID, &msgForgeShares{PaneID: paneID}, "listing shared APIs…")
			return
		case "subscriptions", "subs":
			w.forgeShareDispatch(out, paneID, &msgForgeSubscriptions{PaneID: paneID}, "listing subscriptions…")
			return
		case "subscribe":
			w.handleForgeSubscribe(out, paneID, args[1:])
			return
		case "unsubscribe":
			if len(args) < 2 {
				fmt.Fprintf(out, "\n[forge] usage: ##forge unsubscribe <name>\n")
				return
			}
			w.forgeShareDispatch(out, paneID, &msgForgeUnsubscribe{Name: args[1], PaneID: paneID}, fmt.Sprintf("unsubscribing from %q…", args[1]))
			return
		}
	}

	// Artifact subcommands run against the Forge manager's working directory.
	workDir := w.agSetup.Forge.WorkDir()
	fmt.Fprintln(out)
	if err := forgecmd.Run(workDir, args, out); err != nil {
		fmt.Fprintf(out, "[forge] %v\n", err)
	}
}

// forgeShareDispatch sends an in-process command to the ForgeShareActor (which
// streams its result to the pane), printing an inline "in progress" note. It
// reports a clear error when forged-API sharing is unavailable (upstream off).
func (w *WorkspaceActor) forgeShareDispatch(out *strings.Builder, _ string, m interface{}, note string) {
	if w.forgeSharePID == nil {
		switch {
		case !w.cfg.Upstream.Enabled:
			fmt.Fprintf(out, "\n[forge] forged-API sharing unavailable: upstream is NOT enabled for this workspace.\n"+
				"  Set, under the workspace's upstream block: enabled: true (plus url + api_key). Then restart the daemon.\n")
		case w.agSetup == nil || w.agSetup.Forge == nil || w.agSetup.Scopes == nil:
			fmt.Fprintf(out, "\n[forge] forged-API sharing unavailable: forge/agentic is not initialized.\n"+
				"  A provider api_key is required (set provider.api_key). Then restart the daemon.\n")
		default:
			fmt.Fprintf(out, "\n[forge] forged-API sharing unavailable (no ForgeShareActor for this workspace).\n")
		}
		return
	}
	// In-band reachability check (request/reply uses the same delivery path as the
	// fire-and-forget Send below). This renders SYNCHRONOUSLY in the pane, so it
	// tells us immediately whether the actor processes messages at all — without
	// needing debug logs. If this prints "unreachable", the actor never gets the
	// command (a delivery bug); if it prints "actor ok", the async handlers run and
	// any missing output is downstream (connect/output).
	if _, err := w.system().Root.RequestFuture(w.forgeSharePID, &msgForgePing{}, 2*time.Second).Result(); err != nil {
		fmt.Fprintf(out, "\n[forge] ⚠ ForgeShareActor unreachable: %v (internal delivery issue)\n", err)
	}
	fmt.Fprintf(out, "\n[forge] %s\n", note)
	w.system().Root.Send(w.forgeSharePID, m)
}

func (w *WorkspaceActor) handleForgeShare(out *strings.Builder, paneID string, args []string) {
	// ##forge share list  → show what this session is sharing.
	if len(args) >= 1 && strings.ToLower(args[0]) == "list" {
		w.forgeShareDispatch(out, paneID, &msgForgeShares{PaneID: paneID}, "listing shared APIs…")
		return
	}
	// ##forge share api <name>
	if len(args) < 2 || strings.ToLower(args[0]) != "api" {
		fmt.Fprintf(out, "\n[forge] usage: ##forge share api <name>   (or: ##forge shares to list)\n")
		return
	}
	name := args[1]
	// Synchronous pre-check + feedback: APIOps reads the live forge manager the
	// workspace owns, so this renders RELIABLY via the command output even if the
	// (async) ForgeShareActor or the upstream connect misbehaves. It is also a
	// definitive "am I running the new binary?" marker — older builds never print
	// this line.
	if w.agSetup != nil && w.agSetup.Forge != nil {
		ops := w.agSetup.Forge.APIOps(name)
		if len(ops) == 0 {
			fmt.Fprintf(out, "\n[forge] %q is not enabled — run '##integration enable %s' first (check with '##integration list')\n", name, name)
			return
		}
		fmt.Fprintf(out, "\n[forge] %q: %d op(s) found; handing off to upstream…\n", name, len(ops))
	}
	w.forgeShareDispatch(out, paneID, &msgForgeShareAPI{Name: name, PaneID: paneID}, fmt.Sprintf("sharing api %q (connecting to upstream)…", name))
}

func (w *WorkspaceActor) handleForgeUnshare(out *strings.Builder, paneID string, args []string) {
	// ##forge unshare api <name>
	if len(args) < 2 || strings.ToLower(args[0]) != "api" {
		fmt.Fprintf(out, "\n[forge] usage: ##forge unshare api <name>\n")
		return
	}
	name := args[1]
	w.forgeShareDispatch(out, paneID, &msgForgeUnshareAPI{Name: name, PaneID: paneID}, fmt.Sprintf("unsharing api %q…", name))
}

func (w *WorkspaceActor) handleForgeSubscribe(out *strings.Builder, paneID string, args []string) {
	// ##forge subscribe <name> [--scope pane|panegroup|lane|tab]   (default: tab)
	scopeTok, rest := extractScopeFlag(args)
	if len(rest) < 1 {
		fmt.Fprintf(out, "\n[forge] usage: ##forge subscribe <name> [--scope pane|panegroup|lane|tab]\n")
		return
	}
	name := rest[0]
	kind, ok := agentic.ParseScope(scopeTok)
	if !ok {
		fmt.Fprintf(out, "\n[forge] unknown --scope %q (use pane|panegroup|lane|tab)\n", scopeTok)
		return
	}
	// The SUBSCRIBER chooses the scope; resolve the concrete instance ids from the
	// pane the command was typed in (the source has no say in this).
	ids := w.resolveScopeIDs(paneID)
	w.forgeShareDispatch(out, paneID, &msgForgeSubscribe{Name: name, Kind: kind, IDs: ids, PaneID: paneID},
		fmt.Sprintf("subscribing to %q at scope %s…", name, kind.String()))
}
