package actors

// provider_key.go — where a provider family's API key comes from.
//
// A cross-family selection (`##llm use openai/luna` on an anthropic session)
// needs the SELECTED family's own credentials: cfg.APIKey belongs to the
// configured provider and must never leak into another family's requests. That
// leaves the question of where the other family's key is read from, and rysh has
// two answers that are NOT the same store:
//
//   - the process environment the daemon was started in, and
//   - the ##secret store — session KV → .rysh/secrets/<scope> → rysh.config.yaml.
//
// Resolving from the environment alone made a key registered with `##secret new`
// invisible: `##secret` listed OPENAI_API_KEY, `##llm select` still built the
// provider with an empty key, and every prompt afterwards failed to authenticate.
// The session seat (below) therefore resolves through the secret store first and
// falls back to the environment, which is the last tier of that store anyway.
//
// Scope note: the store itself is workspace-agnostic (the scope is a lookup
// argument), but a workspace must never resolve through another workspace's
// secrets. secretResolver binds the two together so the actors BELOW a
// workspace — tabs, lanes, stacks, panes, humanoids — can resolve a key the
// same way without carrying the WorkspaceActor's state, and without a
// process-global that the last workspace to start would win.

import (
	"os"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// providerKeyNames lists the variable names a provider family's key may be
// stored under, highest precedence first. Every tier is keyed by these same
// names, so one OPENAI_API_KEY resolves whether it was exported into the
// daemon's environment or registered with ##secret.
func providerKeyNames(family string) []string {
	switch family {
	case "openai":
		return []string{"OPENAI_API_KEY"}
	case "ollama":
		return []string{"OLLAMA_API_KEY"} // optional; local ollama needs none
	case "gemini":
		// GEMINI_API_KEY is the documented variable; GOOGLE_API_KEY is accepted
		// as the fallback (same order as config.applyEnvOverrides).
		return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}
	case "claude-cli":
		return nil // the `claude` binary carries its own login; no key
	default:
		return []string{"ANTHROPIC_API_KEY"}
	}
}

// providerKeyEnvHint names the conventional key variable(s) for a provider
// family — the display form of providerKeyNames, for error/warning text.
func providerKeyEnvHint(family string) string {
	names := providerKeyNames(family)
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return names[0] + " (or " + strings.Join(names[1:], ", ") + ")"
	}
}

// providerKeyFromEnv resolves a family's key from the process environment only.
// It serves the override paths that have no secret store in reach (pane,
// humanoid), and backs the nil-store case of providerAPIKey.
func providerKeyFromEnv(family string) string {
	for _, name := range providerKeyNames(family) {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// providerKeyFromStore walks a family's candidate names over one scope chain.
// GetLayered ends every name at the process environment, so that tier is
// covered here too — including when store is nil, which is why the pane and
// humanoid paths degrade to environment-only rather than to nothing.
func providerKeyFromStore(store *namedStore, chain []string, family string) string {
	for _, name := range providerKeyNames(family) {
		if v, _, _, ok := store.GetLayered(chain, name); ok && v != "" {
			return v
		}
	}
	return ""
}

// secretResolver binds the ##secret store to ONE workspace's scope. It is what
// the WorkspaceActor hands down to its tabs, lanes, stacks, panes and
// humanoids: enough to resolve a credential, and nothing more.
//
// A nil *secretResolver is valid and means "no store in reach" — the zero state
// for unit tests and for any actor constructed outside a workspace. It resolves
// from the process environment, which is what every one of these paths did
// before the store was threaded down.
type secretResolver struct {
	store *namedStore
	scope string // the workspace scope token (already sanitized)
}

// scopeChain is the actor-side twin of (*WorkspaceActor).secretScopeChain: the
// tab scope (most specific) then the workspace scope. An empty tabID — a
// humanoid, which belongs to no tab — collapses it to the workspace scope.
func (r *secretResolver) scopeChain(tabID string) []string {
	if tabID == "" {
		return []string{r.scope}
	}
	if tab := sanitizeScope(tabID); tab != r.scope {
		return []string{tab, r.scope}
	}
	return []string{r.scope}
}

// providerAPIKey resolves a provider family's key for a tab under this
// workspace: ##secret store (tab → workspace) first, process environment last.
func (r *secretResolver) providerAPIKey(tabID, family string) string {
	if r == nil {
		return providerKeyFromEnv(family)
	}
	return providerKeyFromStore(r.store, r.scopeChain(tabID), family)
}

// providerAPIKey resolves a family's key the way every other rysh credential
// resolves: through the ##secret store for the scope chain of the tab the
// command was typed in (tab → workspace), then the process environment.
func (w *WorkspaceActor) providerAPIKey(family string) string {
	if w.secrets == nil {
		return providerKeyFromEnv(family)
	}
	return providerKeyFromStore(w.secrets, w.secretScopeChain(""), family)
}

// missingProviderKey reports whether selecting family would leave the provider
// with no credentials, and names the variable to set for it. The single answer
// behind both the selection-time warning and the picker's set-a-secret step, so
// the two cannot disagree about whether a key is in reach.
//
// A family that matches the configured provider is never "missing": it runs on
// cfg.APIKey, the credential the session was started with.
func (w *WorkspaceActor) missingProviderKey(family string) (keyName string, missing bool) {
	names := providerKeyNames(family)
	if len(names) == 0 || !provider.RequiresAPIKey(family) ||
		family == providerFamily(w.cfg.ProviderName) ||
		w.providerAPIKey(family) != "" {
		return "", false
	}
	return names[0], true
}

// childSecretResolver is the resolver handed to every actor this workspace
// spawns. Nil when the workspace has no store, so children stay environment-only
// rather than silently resolving against an empty one.
func (w *WorkspaceActor) childSecretResolver() *secretResolver {
	if w.secrets == nil {
		return nil
	}
	scope, _ := w.secretWorkspaceScope()
	return &secretResolver{store: w.secrets, scope: scope}
}
