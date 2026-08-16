// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handlePaneGroupCommand implements the `##panegroup` / `##pg` / `##stack`
// command family. A pane group is exactly a stack of panes, which is why
// `stack` is offered as an alias.

func (w *WorkspaceActor) handlePaneGroupCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		sub = "info"
	}
	switch sub {
	case "list":
		w.cmdPaneGroupList(out, paneID)
	case "info":
		// ##panegroup info         -> the caller's stack
		// ##panegroup info <ref>   -> THAT stack (F-55; see workspace_info_ref.go)
		w.cmdPaneGroupInfo(out, paneID, args)
	case "layout":
		w.cmdPaneGroupLayout(out, paneID)
	case "model", "llm":
		// ##stack model — bind a model for the active pane group; every
		// pane in it that has not chosen for itself follows.
		w.handleScopeModelCommand(out, paneID, scopeStack, args[1:])
	case "delete":
		// ##panegroup delete <group-id>  -> delete the pane group with that id (see ##panegroup list)
		if len(args) < 2 {
			ryshWriter(out).UsageLine("##panegroup delete <group-id>   (see ##panegroup list)")
			w.failRyshUsage("usage: %s", "##panegroup delete <group-id>   (see ##panegroup list)")
		} else {
			resp := w.handleCLIDeletePaneGroup(&msg.MsgCLIDeletePaneGroup{PaneGroupID: args[1]})
			if resp.OK {
				fmt.Fprintf(out, "\n[rysh] pane group %s deleted\n", args[1])
			} else {
				fmt.Fprintf(out, "\n[rysh] %s\n", resp.Error)
				w.failRysh("%s", resp.Error)
			}
		}
	default:
		ryshWriter(out).Unknown("panegroup", sub,
			"##panegroup info [<stack>]  the caller's stack, or the one named (id or index)",
			"##panegroup list     list pane groups in the active tab",
			"##panegroup layout   show lane layout overview",
			"##panegroup model [<p>/<name>]  bind the stack's LLM model (aliases: ##stack, ##pg)",
			"##panegroup delete <group-id>  delete a pane group by id",
		)
		w.failRyshUsage("unknown subcommand for ##%s: %q", "panegroup", sub)
	}
}
