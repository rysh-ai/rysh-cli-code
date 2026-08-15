// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/forge"
	"github.com/rysh-ai/rysh-cli-code/internal/mcp"
)

// replayScope re-applies any persisted scoped integration / MCP-server enablement
// for the given scope instance, when a Tab/Lane/PaneGroup/Pane actor (re)starts.
// On a fresh session the scope-instance ids are new, so nothing matches and it is
// a no-op; on detach/attach or restart the ids are restored verbatim, so an
// integration/server enabled `--scope lane` lands back on the same lane. Forge
// enable is fast/local (replayed inline); MCP replay connects to servers (network
// I/O), so it runs off the actor mailbox to avoid stalling the restoring actor.
func replayScope(s *agentic.Setup, kind agentic.ScopeKind, ids agentic.ScopeIDs) {
	if s == nil || s.Scopes == nil {
		return
	}
	key := agentic.ScopeKey(kind, ids)
	if key == "" || key == "global" {
		return
	}
	reg := s.Scopes.RegistryFor(kind, ids)
	if s.Forge != nil {
		s.Forge.ReplayScope(context.Background(), key, forge.ScopeTarget{Key: key, Registry: reg})
	}
	if s.MCP != nil {
		go s.MCP.ReplayScope(context.Background(), key, mcp.ScopeTarget{Key: key, Registry: reg})
	}
}

// extractScopeFlag pulls an optional `--scope <value>` / `--scope=<value>` out of
// args and returns the scope token plus the remaining args (with the flag
// removed), so a command parser that doesn't know about --scope sees clean args.
func extractScopeFlag(args []string) (scope string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--scope" || a == "-scope":
			if i+1 < len(args) {
				scope = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--scope="):
			scope = strings.TrimPrefix(a, "--scope=")
		default:
			rest = append(rest, a)
		}
	}
	return scope, rest
}

// teardownScope retracts any integration/MCP tools enabled at the given scope
// instance and drops the scope's registry. Tab/Lane/PaneGroup/Pane actors call
// it from their *actor.Stopping handler so scoped tools don't linger after the
// owning entity closes. Because actor teardown cascades (a tab stops its lanes,
// which stop their groups, which stop their panes), each level retracts its own
// scope, leaving no stale parent links.
func teardownScope(s *agentic.Setup, kind agentic.ScopeKind, id string) {
	if s == nil || id == "" {
		return
	}
	key := agentic.ScopeKeyByID(kind, id)
	if s.Forge != nil {
		s.Forge.UnregisterScope(key)
	}
	if s.MCP != nil {
		s.MCP.UnregisterScope(key)
	}
	if s.Scopes != nil {
		s.Scopes.Close(kind, id)
	}
}
