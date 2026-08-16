// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handleLaneCommand implements the `##lane` command family: listing lanes,
// inspecting one, renaming, resizing and model binding.

func (w *WorkspaceActor) handleLaneCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		sub = "list"
	}
	switch sub {
	case "list":
		w.cmdLaneList(out)
	case "info":
		// ##lane info          -> the caller's lane
		// ##lane info <ref>    -> THAT lane (F-55; see workspace_info_ref.go)
		w.cmdLaneInfo(out, paneID, args)
	case "model", "llm":
		// ##lane model — binds every stack/pane in the active lane that
		// has not chosen a narrower model of its own.
		w.handleScopeModelCommand(out, paneID, scopeLane, args[1:])
	case "name":
		// ##lane name <lane-name>
		if len(args) < 2 {
			ryshWriter(out).UsageLine("##lane name <lane-name>")
			w.failRyshUsage("usage: %s", "##lane name <lane-name>")
		} else {
			name := strings.Join(args[1:], " ")
			if w.renameActiveLane(name) {
				fmt.Fprintf(out, "\n[rysh] active lane renamed to %q\n", name)
			} else {
				fmt.Fprintf(out, "\n[rysh] no active lane to rename\n")
				w.failRysh("no active lane to rename")
			}
		}
	case "delete":
		// ##lane delete <lane-id>  -> delete the lane with that id (see ##lane list)
		if len(args) < 2 {
			ryshWriter(out).UsageLine("##lane delete <lane-id>   (see ##lane list)")
			w.failRyshUsage("usage: %s", "##lane delete <lane-id>   (see ##lane list)")
		} else {
			resp := w.handleCLIDeleteLane(&msg.MsgCLIDeleteLane{LaneID: args[1]})
			if resp.OK {
				fmt.Fprintf(out, "\n[rysh] lane %s deleted\n", args[1])
			} else {
				fmt.Fprintf(out, "\n[rysh] %s\n", resp.Error)
				w.failRysh("%s", resp.Error)
			}
		}
	default:
		ryshWriter(out).Unknown("lane", sub,
			"##lane list",
			"##lane info [<lane>]   the caller's lane, or the one named (id, index or name)",
			"##lane name <lane-name>",
			"##lane model [<provider>/<name>]  bind the lane's LLM model",
			"##lane delete <lane-id>",
		)
		w.failRyshUsage("unknown subcommand for ##%s: %q", "lane", sub)
	}
}
