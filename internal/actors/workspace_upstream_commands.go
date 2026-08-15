// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// handleUpstreamCommand implements the `##upstream` command family: connecting
// this session to a remote rysh server, and inspecting that connection.

func (w *WorkspaceActor) handleUpstreamCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		sub = "status"
	}
	switch sub {
	case "connect":
		w.handleUpstreamConnect(out, args[1:])
	case "status":
		if !w.cfg.Upstream.Enabled {
			// Answering "it is off" IS the status. Only ACTIONS that need
			// upstream report failure — same rule as ##replay status.
			fmt.Fprintf(out, "\n[rysh] upstream is not enabled\n")
			fmt.Fprintf(out, "  run: ##upstream connect <url> <api-key>\n")
			fmt.Fprintf(out, "  (or set upstream.enabled: true in rysh.config.yaml by hand)\n\n")
		} else {
			// Count the panes currently shared upstream. Only the active tab is
			// consulted, which is what this command has always reported.
			sharedCount := 0
			if tab := w.currentTab(); tab != nil {
				_, sharedCount = domain.PaneIDsInTabWhere(
					w.queryTabSnapshot(tab.id),
					func(p *domain.PaneSnapshot) bool { return !p.Sharing },
				)
			}

			o := ryshWriter(out)
			o.Header("upstream status")
			o.Rule()
			o.Field("enabled", "true")
			o.Field("url", "%s", w.cfg.Upstream.URL)
			o.Field("workspace", "%s", w.cfg.Upstream.WorkspaceName())
			o.Field("auto_share", "%v", w.cfg.Upstream.AutoShare)
			o.Field("shared panes", "%d", sharedCount)
			o.Rule()
		}
	case "my-shares":
		w.handleUpstreamShares(out)
	case "list-remote":
		w.handleUpstreamListRemote(out)
	case "subscribe":
		shareID := ""
		subMode := "" // optional: "view" or "control"
		if len(args) > 1 {
			shareID = args[1]
		}
		if len(args) > 2 {
			subMode = args[2]
		}
		w.handleUpstreamSubscribe(out, paneID, shareID, subMode)
	case "unsubscribe":
		unsubArg := ""
		if len(args) > 1 {
			unsubArg = args[1]
		}
		w.handleUpstreamUnsubscribe(out, unsubArg)
	case "send":
		text := ""
		if len(args) > 1 {
			text = strings.Join(args[1:], " ")
		}
		w.handleUpstreamSend(out, text)
	default:
		ryshWriter(out).Unknown("upstream", sub,
			"##upstream connect <url> <api-key>  connect this session to a rysh server",
			"  (resolves the workspace id from the key and writes rysh.config.yaml;",
			"   leaves upstream.governance off — that is a separate opt-in)",
			"##upstream status              show upstream configuration and status",
			"##upstream my-shares           list shares published from this session",
			"##upstream list-remote         list all shares in the workspace (from server)",
			"##upstream subscribe <shareID> [view|control]  subscribe to a remote share",
			"  (a tab/lane/pane_group share opens a mirror tab; control mode lets you",
			"   focus a pane in it and run commands on the source pane)",
			"##upstream unsubscribe [shareID]  stop a remote subscription / mirror tab",
			"##upstream send <text>         send command to active remote share",
			"####<command>                  run ##<command> on the shared source pane",
			"                               (control-mode subscriber only; e.g. ####new grid 2x2)",
		)
		w.failRyshUsage("unknown subcommand for ##%s: %q", "upstream", sub)
	}
}
