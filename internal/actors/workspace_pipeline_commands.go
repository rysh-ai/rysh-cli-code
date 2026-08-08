package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handlePipelineCommand implements the `##pipe` / `##pipeline` command family:
// loading and building a tab's pipeline, running it, and managing the lane
// placeholders it is wired through.

func (w *WorkspaceActor) handlePipelineCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		sub = "help"
	}
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[pipeline] no active tab\n")
		w.failRysh("no active tab")
	} else {
		tabSnap := w.queryTabSnapshot(tab.id)
		if tabSnap == nil || !tabSnap.PipelineEnabled {
			fmt.Fprintf(out, "\n[pipeline] this tab is not enabled for pipeline development\n")
			w.failRysh("this tab is not enabled for pipeline development")
			fmt.Fprintf(out, "  run: ##tab pipeline enable\n\n")
		} else {
			pipeArgs := ""
			if len(args) > 1 {
				pipeArgs = strings.Join(args[1:], " ")
			}
			switch sub {
			case "help":
				cmdPipelineHelp(out)
			case "list":
				cmdPipelineList(out, tab.actor, pipeArgs)
			case "load":
				cmdPipelineLoad(out, tab.actor, pipeArgs)
			case "unload":
				cmdPipelineUnload(out, tab.actor, pipeArgs)
			case "show":
				cmdPipelineShow(out, tab.actor, pipeArgs)
			case "build":
				cmdPipelineBuild(out, tab.actor, pipeArgs)
			case "run":
				w.forwardToActiveTab(&msg.MsgPipelineCommand{PaneID: paneID, Cmd: "run", Args: pipeArgs})
			case "status":
				cmdPipelineStatus(out, tab.actor)
			case "clear":
				cmdPipelineClear(out, tab.actor)
			case "name":
				if len(args) < 2 {
					ryshWriter(out).UsageLineIn("pipeline", "##pipe name <pipeline-name>")
					w.failRyshUsage("usage: %s", "##pipe name <pipeline-name>")
				} else {
					name := args[1]
					tab.actor.pipelineName = name
					w.persistToKV()
					fmt.Fprintf(out, "\n[pipeline] pipeline name set to %q\n", name)
				}
			case "placeholder":
				action := ""
				if len(args) > 1 {
					action = args[1]
				}
				switch action {
				case "add":
					w.forwardToActiveTab(&msg.MsgTabCreatePane{Title: w.generateUniqueAlias()})
					w.syncActivePane()
					w.persistToKV()
					fmt.Fprintf(out, "\n[pipeline] added new placeholder (lane)\n")
				case "list":
					w.cmdPipelinePlaceholderList(out, tab)
				default:
					ryshWriter(out).UsageIn("pipeline",
						"##pipe placeholder add    add a new pipeline placeholder (lane)",
						"##pipe placeholder list   list current placeholders",
					)
				}
			default:
				ryshWriter(out).UnknownIn("pipeline", sub)
				w.failRyshUsage("unknown %s subcommand: %q", "pipeline", sub)
				cmdPipelineHelp(out)
			}
		}
	}
}
