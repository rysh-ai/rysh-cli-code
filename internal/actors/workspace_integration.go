package actors

import (
	"context"
	"fmt"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/forge"
)

// handleIntegrationSubcommand implements the `##integration` command family:
// enabling, listing, inspecting, and removing Forge-generated API integrations
// whose tools are exposed to agents. Integrations are added/generated with the
// `rysh forge` CLI; this enables them live in the running session.
//
//	##integration list
//	##integration enable <name> [--scope pane|panegroup|lane|tab]   (default: tab)
//	##integration disable <name>
//	##integration tools <name>
//	##integration remove <name>
func (w *WorkspaceActor) handleIntegrationSubcommand(out *strings.Builder, paneID string, args []string) {
	if w.agSetup == nil || w.agSetup.Forge == nil {
		fmt.Fprintf(out, "\n[integration] Forge is unavailable (agentic mode disabled?)\n")
		return
	}
	mgr := w.agSetup.Forge

	if len(args) == 0 || args[0] == "help" {
		w.integrationHelp(out)
		return
	}

	switch args[0] {
	case "list", "ls":
		w.integrationList(out, mgr)

	case "enable":
		name, scopeStr, ok := parseNameAndScope(args[1:])
		if !ok {
			fmt.Fprintf(out, "\n[integration] usage: ##integration enable <name> [--scope pane|panegroup|lane|tab]\n")
			return
		}
		kind, ok := agentic.ParseScope(scopeStr)
		if !ok {
			fmt.Fprintf(out, "\n[integration] unknown scope %q (use pane|panegroup|lane|tab)\n", scopeStr)
			return
		}
		ids := w.resolveScopeIDs(paneID)
		target := forge.ScopeTarget{
			Key:      agentic.ScopeKey(kind, ids),
			Registry: w.agSetup.Scopes.RegistryFor(kind, ids),
		}
		n, mode, err := mgr.EnableByName(context.Background(), name, target)
		if err != nil {
			fmt.Fprintf(out, "\n[integration] enable %q failed: %v\n", name, err)
			return
		}
		fmt.Fprintf(out, "\n[integration] %q enabled at scope %s — %d tool(s) registered (exposure: %s). Visible to AI executing in that scope on its next prompt.\n", name, kind, n, modeStr(string(mode)))

	case "disable":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[integration] usage: ##integration disable <name>\n")
			return
		}
		if err := mgr.Disable(args[1]); err != nil {
			fmt.Fprintf(out, "\n[integration] %v\n", err)
			return
		}
		fmt.Fprintf(out, "\n[integration] %q disabled (tools unregistered)\n", args[1])

	case "tools":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[integration] usage: ##integration tools <name>\n")
			return
		}
		names, ok := mgr.Tools(args[1])
		if !ok {
			fmt.Fprintf(out, "\n[integration] %q is not enabled\n", args[1])
			return
		}
		fmt.Fprintf(out, "\n[integration] tools for %q (%d):\n", args[1], len(names))
		for _, n := range names {
			fmt.Fprintf(out, "  %s\n", n)
		}

	case "remove", "rm", "delete":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[integration] usage: ##integration remove <name>\n")
			return
		}
		if err := mgr.Remove(args[1]); err != nil {
			fmt.Fprintf(out, "\n[integration] %v\n", err)
			return
		}
		fmt.Fprintf(out, "\n[integration] removed %q (tools unregistered, definition + spec deleted)\n", args[1])

	default:
		fmt.Fprintf(out, "\n[integration] unknown subcommand: %q\n", args[0])
		w.integrationHelp(out)
	}
}

func (w *WorkspaceActor) integrationList(out *strings.Builder, mgr *forge.Manager) {
	items := mgr.List()
	if len(items) == 0 {
		fmt.Fprint(out, progname.Rewrite("\n[integration] none configured. Add one with the CLI: rysh forge add <name> <spec-file>\n"))
		return
	}
	fmt.Fprintf(out, "\n[integration] integrations (%d):\n", len(items))
	for _, s := range items {
		marker := "○"
		state := "disabled"
		if s.Enabled {
			marker = "●"
			state = fmt.Sprintf("%d tool(s), %d op(s), %s, scope=%s", s.ToolCount, s.Operations, modeStr(s.Mode), scopeLabel(s.Scope))
		}
		if s.Error != "" {
			state = "error: " + s.Error
		}
		fmt.Fprintf(out, "  %s %-20s %-8s %s\n", marker, s.Name, s.Source, state)
	}
}

func (w *WorkspaceActor) integrationHelp(out *strings.Builder) {
	fmt.Fprintf(out, "\n[integration] Forge API integrations — expose generated API tools to agents\n")
	fmt.Fprintf(out, "  ##integration list                list configured integrations + status\n")
	fmt.Fprintf(out, "  ##integration enable <name> [--scope pane|panegroup|lane|tab]   (default: tab)\n")
	fmt.Fprintf(out, "  ##integration disable <name>      unregister an integration's tools\n")
	fmt.Fprintf(out, "  ##integration tools <name>        list an enabled integration's tools\n")
	fmt.Fprintf(out, "  ##integration remove <name>       disable + delete an integration\n")
	fmt.Fprint(out, progname.Rewrite("  Add/generate integrations with the CLI:  rysh forge add <name> <spec-file>\n"))
	fmt.Fprintf(out, "  Large APIs expose 3 dynamic meta-tools (list/get-schema/invoke) to stay in budget.\n")
}

// modeStr labels an exposure mode for display ("" ⇒ resolved static).
func modeStr(s string) string {
	if s == "" {
		return "static"
	}
	return s
}

// scopeLabel shortens a scope key ("lane:<uuid>") for display ("lane:abcd1234").
func scopeLabel(key string) string {
	if key == "" {
		return "global"
	}
	kind, id, found := strings.Cut(key, ":")
	if !found || id == "" {
		return key
	}
	if len(id) > 8 {
		id = id[:8]
	}
	return kind + ":" + id
}

// parseNameAndScope extracts the positional <name> and an optional
// `--scope <value>` / `--scope=<value>` from the args after the subcommand.
// Returns ok=false when no name is present or --scope is given without a value.
func parseNameAndScope(args []string) (name, scope string, ok bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--scope" || a == "-scope":
			if i+1 >= len(args) {
				return "", "", false
			}
			scope = args[i+1]
			i++
		case strings.HasPrefix(a, "--scope="):
			scope = strings.TrimPrefix(a, "--scope=")
		case strings.HasPrefix(a, "-"):
			// ignore unknown flags
		case name == "":
			name = a
		}
	}
	if name == "" {
		return "", "", false
	}
	return name, scope, true
}

// resolveScopeIDs maps a pane id to its full scope chain (tab/lane/group/pane)
// by walking the active tab snapshot — the "execution point" of the command.
func (w *WorkspaceActor) resolveScopeIDs(paneID string) agentic.ScopeIDs {
	ids := agentic.ScopeIDs{PaneID: paneID}
	tab := w.currentTab()
	if tab == nil {
		return ids
	}
	ids.TabID = tab.id
	if paneID == "" {
		return ids
	}
	snap := w.queryTabSnapshot(tab.id)
	if snap == nil {
		return ids
	}
	for li := range snap.Lanes {
		for _, g := range snap.Lanes[li].PaneGroups {
			for _, p := range g.Panes {
				if p.ID == paneID {
					ids.LaneID = snap.Lanes[li].ID
					ids.GroupID = g.ID
					return ids
				}
			}
		}
	}
	return ids
}
