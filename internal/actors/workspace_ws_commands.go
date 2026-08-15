// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/asynkron/protoactor-go/actor"
)

// handleWorkspaceCommand implements the `##workspace` / `##ws` command family:
// listing, creating, switching between and renaming workspaces.

func (w *WorkspaceActor) handleWorkspaceCommand(ctx actor.Context, out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" {
		sub = "list"
	}
	switch sub {
	case "list":
		fmt.Fprintf(out, "\n[rysh] workspaces (%d total)\n", len(w.workspaceNames))
		ryshWriter(out).Rule()
		for i, n := range w.workspaceNames {
			marker := "  "
			if i == w.workspaceIdx {
				marker = "> "
			}
			fmt.Fprintf(out, "%s[%d] %s\n", marker, i+1, n)
		}
		ryshWriter(out).Rule()
		fmt.Fprintf(out, "  switch with Ctrl+W (1-9 / arrows)\n")
	case "model", "llm":
		// ##ws model — binds every tab/lane/stack/pane in this workspace
		// that has not chosen a narrower model of its own.
		w.handleScopeModelCommand(out, paneID, scopeWorkspace, args[1:])
	case "create":
		// ##ws create <name> <api_key>  -> spawn a new workspace (wd=~/,
		// upstream enabled with the api_key) and persist it to rysh.config.yaml.
		if len(args) < 3 {
			ryshWriter(out).UsageLine("##ws create <name> <api_key>")
			w.failRyshUsage("usage: %s", "##ws create <name> <api_key>")
			break
		}
		name := args[1]
		apiKey := args[2]
		exists := false
		for _, n := range w.workspaceNames {
			if n == name {
				exists = true
				break
			}
		}
		if exists {
			fmt.Fprintf(out, "\n[rysh] workspace %q already exists\n", name)
			w.failRysh("workspace %q already exists", name)
			break
		}
		ctx.Send(ctx.Parent(), &createWorkspaceMsg{name: name, apiKey: apiKey})
		fmt.Fprintf(out, "\n[rysh] creating workspace %q (working_directory=~/, upstream key set)\n", name)
		fmt.Fprintf(out, "  it is added to rysh.config.yaml; switch to it with Ctrl+W\n")
	case "cwd", "cd", "chdir":
		// ##workspace cwd [<path>] -- show/set the ACTIVE workspace's working
		// directory for newly created panes. Join args[1:] so unquoted paths
		// with spaces still work.
		dir := ""
		if len(args) > 1 {
			dir = strings.Join(args[1:], " ")
		}
		w.cmdWorkspaceCwd(out, dir)
	default:
		ryshWriter(out).Unknown("ws", sub,
			"##ws list                     list workspaces",
			"##ws cwd [<path>]             show/set the active workspace's working dir",
			"##ws model [<p>/<name>]       bind the workspace's LLM model",
			"##ws create <name> <api_key>  create a workspace (path ~/) and persist it",
		)
		w.failRyshUsage("unknown subcommand for ##%s: %q", "ws", sub)
	}
}
