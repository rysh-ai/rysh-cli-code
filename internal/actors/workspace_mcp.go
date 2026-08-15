// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/mcp"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handleMCPSubcommand implements the `##mcp` command family: connecting to,
// listing, inspecting, and removing external Model Context Protocol servers
// whose tools are exposed to rysh agents.
//
//	##mcp add <name> http <url> [--header K:V] [--approve] [--prefix P] [--max-tools N]
//	##mcp add <name> stdio <command> [args...] [--env K=V] [--approve] [--prefix P]
//	##mcp list
//	##mcp tools <name>
//	##mcp reconnect <name>
//	##mcp remove <name>
func (w *WorkspaceActor) handleMCPSubcommand(out *strings.Builder, paneID string, args []string) {
	if w.agSetup == nil || w.agSetup.MCP == nil {
		fmt.Fprintf(out, "\n[mcp] MCP is unavailable (agentic mode disabled?)\n")
		w.failRysh("MCP is unavailable (agentic mode disabled?)")
		return
	}
	mgr := w.agSetup.MCP

	if len(args) == 0 || args[0] == "help" {
		w.mcpHelp(out)
		return
	}

	switch args[0] {
	case "list", "ls":
		w.mcpList(out, mgr)

	case "tools":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("mcp", "##mcp tools <name>")
			w.failRyshUsage("usage: %s", "##mcp tools <name>")
			return
		}
		w.mcpTools(out, mgr, args[1])

	case "add":
		scopeStr, rest := extractScopeFlag(args[1:])
		kind, sok := agentic.ParseScope(scopeStr)
		if !sok {
			fmt.Fprintf(out, "\n[mcp] unknown scope %q (use pane|panegroup|lane|tab)\n", scopeStr)
			w.failRysh("unknown scope %q (use pane|panegroup|lane|tab)", scopeStr)
			return
		}
		def, err := mcp.ParseAddArgs(rest)
		if err != nil {
			w.failRysh("%v", err)
			fmt.Fprintf(out, "\n[mcp] %v\n", err)
			w.mcpHelp(out)
			return
		}
		// Resolve the scope target on the actor goroutine (snapshot query), then
		// connect off-mailbox capturing only thread-safe values.
		ids := w.resolveScopeIDs(paneID)
		target := mcp.ScopeTarget{Key: agentic.ScopeKey(kind, ids), Registry: w.agSetup.Scopes.RegistryFor(kind, ids)}
		fmt.Fprintf(out, "\n[mcp] connecting to %q (%s: %s) at scope %s…\n", def.Name, def.Transport, def.Detail(), kind)
		pub := w.pub
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			n, err := mgr.AddServerScoped(ctx, def, target)
			if err != nil {
				emitMCP(pub, paneID, fmt.Sprintf("[mcp] %q failed to connect: %v (definition saved; will retry on restart)\n", def.Name, err))
				return
			}
			emitMCP(pub, paneID, fmt.Sprintf("[mcp] %q connected — registered %d tool(s) at scope %s. Visible to AI executing in that scope on its next prompt.\n", def.Name, n, kind))
		}()

	case "reconnect":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("mcp", "##mcp reconnect <name>")
			w.failRyshUsage("usage: %s", "##mcp reconnect <name>")
			return
		}
		name := args[1]
		fmt.Fprintf(out, "\n[mcp] reconnecting %q…\n", name)
		pub := w.pub
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			n, err := mgr.Reconnect(ctx, name)
			if err != nil {
				emitMCP(pub, paneID, fmt.Sprintf("[mcp] %q reconnect failed: %v\n", name, err))
				return
			}
			emitMCP(pub, paneID, fmt.Sprintf("[mcp] %q reconnected — %d tool(s).\n", name, n))
		}()

	case "remove", "rm", "delete":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("mcp", "##mcp remove <name>")
			w.failRyshUsage("usage: %s", "##mcp remove <name>")
			return
		}
		name := args[1]
		if err := mgr.RemoveServer(name); err != nil {
			w.failRysh("%v", err)
			fmt.Fprintf(out, "\n[mcp] %v\n", err)
			return
		}
		fmt.Fprintf(out, "\n[mcp] removed %q (tools unregistered, definition deleted)\n", name)

	default:
		ryshWriter(out).UnknownIn("mcp", args[0])
		w.failRyshUsage("unknown %s subcommand: %q", "mcp", args[0])
		w.mcpHelp(out)
	}
}

func (w *WorkspaceActor) mcpList(out *strings.Builder, mgr *mcp.Manager) {
	servers := mgr.List()
	if len(servers) == 0 {
		fmt.Fprintf(out, "\n[mcp] no MCP servers configured. Add one with: ##mcp add <name> http <url>\n")
		return
	}
	fmt.Fprintf(out, "\n[mcp] servers (%d):\n", len(servers))
	for _, s := range servers {
		marker := "○"
		state := "disconnected"
		if s.Connected {
			marker = "●"
			state = fmt.Sprintf("%d tool(s)", s.Registered)
			if s.Discovered > s.Registered {
				state += fmt.Sprintf(" (%d discovered, capped)", s.Discovered)
			}
		} else if s.Error != "" {
			state = "error: " + s.Error
		}
		fmt.Fprintf(out, "  %s %-16s %-6s %s — %s\n", marker, s.Name, s.Transport, state, s.Detail)
	}
}

func (w *WorkspaceActor) mcpTools(out *strings.Builder, mgr *mcp.Manager, name string) {
	tools, ok := mgr.ToolsOf(name)
	if !ok {
		fmt.Fprintf(out, "\n[mcp] no server named %q (see ##mcp list)\n", name)
		return
	}
	if len(tools) == 0 {
		fmt.Fprintf(out, "\n[mcp] %q exposes no registered tools\n", name)
		return
	}
	fmt.Fprintf(out, "\n[mcp] tools for %q (%d):\n", name, len(tools))
	for _, t := range tools {
		desc := t.Description
		if len(desc) > 70 {
			desc = desc[:69] + "…"
		}
		fmt.Fprintf(out, "  %-28s (%s)  %s\n", t.RegisteredName, t.RemoteName, desc)
	}
}

func (w *WorkspaceActor) mcpHelp(out *strings.Builder) {
	fmt.Fprintf(out, "\n[mcp] Model Context Protocol servers — expose external tools to agents\n")
	fmt.Fprintf(out, "  ##mcp add <name> http <url> [flags]    connect a Streamable-HTTP MCP server\n")
	fmt.Fprintf(out, "  ##mcp add <name> stdio <cmd> [args]    spawn a stdio MCP server\n")
	fmt.Fprintf(out, "  ##mcp list                             list configured servers + status\n")
	fmt.Fprintf(out, "  ##mcp tools <name>                     list a server's registered tools\n")
	fmt.Fprintf(out, "  ##mcp reconnect <name>                 reconnect a server\n")
	fmt.Fprintf(out, "  ##mcp remove <name>                    disconnect + forget a server\n")
	fmt.Fprintf(out, "  flags: --scope pane|panegroup|lane|tab (default tab) --approve --prefix P --max-tools N\n")
	fmt.Fprintf(out, "         --header K:V (http, repeatable) --env K=V (stdio, repeatable)\n")
	fmt.Fprintf(out, "  e.g. ##mcp add weather http http://localhost:8081/mcp\n")
	fmt.Fprintf(out, "       ##mcp add fs stdio npx -y @modelcontextprotocol/server-filesystem /tmp\n")
	fmt.Fprintf(out, "  servers persist in .rysh/mcp.json and reconnect on startup.\n")
}

// emitMCP publishes an asynchronous MCP result to both the merged output buffer
// (visible in shell/prompt modes) and the rysh buffer, matching how explicit
// ## commands surface their output.
func emitMCP(pub *msg.NATSPublisher, paneID, text string) {
	_ = pub.SendPaneOutput(paneID, text)
	_ = pub.SendPaneRyshOutput(paneID, text)
}
