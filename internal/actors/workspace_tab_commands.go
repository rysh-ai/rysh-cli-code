package actors

import (
	"fmt"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handleTabCommand implements the `##tab` command family: listing tabs and
// their panes, renaming, pipeline mode, model binding and deletion.

func (w *WorkspaceActor) handleTabCommand(ctx actor.Context, out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		sub = "list"
	}
	switch sub {
	case "list":
		fmt.Fprintf(out, "\n[rysh] tabs (%d total)\n", len(w.tabs))
		ryshWriter(out).Rule()
		for i, t := range w.tabs {
			marker := "  "
			if i == w.activeTabIdx {
				marker = "> "
			}
			paneCount := w.queryPaneCount(t.id)
			pipeLabel := ""
			tabSnap := w.queryTabSnapshot(t.id)
			if tabSnap != nil && tabSnap.PipelineEnabled {
				pipeLabel = "  [pipeline]"
			}
			fmt.Fprintf(out, "%s[%d] %-20s  id=%-36s  panes=%d%s\n",
				marker, i+1, t.title, t.id, paneCount, pipeLabel)
		}
		ryshWriter(out).Rule()
	case "model", "llm":
		// ##tab model — binds every lane/stack/pane in the active tab
		// that has not chosen a narrower model of its own.
		w.handleScopeModelCommand(out, paneID, scopeTab, args[1:])
	case "list-panes":
		// ##tab list-panes [--tab <tab-id>]  -> list panes of a tab
		// (defaults to the active tab when --tab is omitted).
		tabArg := extractTabFlag(args[1:])
		target := w.resolveTabArg(tabArg)
		if target == nil {
			if tabArg == "" {
				fmt.Fprintf(out, "\n[rysh] no active tab\n")
				w.failRysh("no active tab")
			} else {
				fmt.Fprintf(out, "\n[rysh] tab not found: %s\n", tabArg)
				w.failRysh("tab not found: %s", tabArg)
			}
			break
		}
		w.cmdPaneListInTab(out, target)
	case "name":
		// ##tab name <new-name>             -> rename the active tab
		// ##tab name <tab-id> <new-name...>  -> rename the tab with that id
		if len(args) < 2 {
			ryshWriter(out).Usage(
				"##tab name <new-name>            rename the active tab",
				"##tab name <tab-id> <new-name>   rename the tab with that id (see ##tab list)",
			)
			w.failRyshUsage("##tab name needs a name")
		} else if len(args) >= 3 && w.tabInfoByID(args[1]) != nil {
			// Targeted rename: first arg is a known tab id, rest is the name.
			targetID := args[1]
			name := strings.Join(args[2:], " ")
			if w.renameTabByID(targetID, name) {
				fmt.Fprintf(out, "\n[rysh] tab %s renamed to %q\n", targetID, name)
			} else {
				fmt.Fprintf(out, "\n[rysh] could not rename tab %s\n", targetID)
				w.failRysh("could not rename tab %s", targetID)
			}
		} else {
			name := strings.Join(args[1:], " ")
			if w.renameActiveTab(name) {
				fmt.Fprintf(out, "\n[rysh] tab renamed to %q\n", name)
			} else {
				fmt.Fprintf(out, "\n[rysh] no active tab to rename\n")
				w.failRysh("no active tab to rename")
			}
		}
	case "pipeline":
		// ##tab pipeline enable|disable
		action := ""
		if len(args) > 1 {
			action = args[1]
		}
		switch action {
		case "enable":
			w.forwardToActiveTab(&msg.MsgTabPipelineEnable{})
			fmt.Fprintf(out, "\n[rysh] pipeline mode enabled for the active tab\n")
		case "disable":
			w.forwardToActiveTab(&msg.MsgTabPipelineDisable{})
			fmt.Fprintf(out, "\n[rysh] pipeline mode disabled for the active tab\n")
		default:
			ryshWriter(out).Usage(
				"##tab pipeline enable    enable pipeline mode for the active tab",
				"##tab pipeline disable   disable pipeline mode for the active tab",
			)
			w.failRyshUsage("unknown ##tab pipeline action")
		}
	case "delete":
		// ##tab delete <tab-id>  -> delete the tab with that id (see ##tab list)
		if len(args) < 2 {
			ryshWriter(out).UsageLine("##tab delete <tab-id>   (see ##tab list)")
			w.failRyshUsage("usage: %s", "##tab delete <tab-id>   (see ##tab list)")
		} else {
			resp := w.handleCLIDeleteTab(ctx, &msg.MsgCLIDeleteTab{TabID: args[1]})
			if resp.OK {
				fmt.Fprintf(out, "\n[rysh] tab %s deleted\n", args[1])
			} else {
				fmt.Fprintf(out, "\n[rysh] %s\n", resp.Error)
				w.failRysh("%s", resp.Error)
			}
		}
	default:
		ryshWriter(out).Unknown("tab", sub,
			"##tab list",
			"##tab list-panes [--tab <tab-id>]",
			"##tab name <tab-name>",
			"##tab model [<provider>/<name>]  bind the tab's LLM model",
			"##tab delete <tab-id>",
			"##tab pipeline enable|disable",
		)
		w.failRyshUsage("unknown subcommand for ##%s: %q", "tab", sub)
	}
}
