package actors

// ##variable — the environment-variable flavour of the named-value store (see
// store.go for the shared machinery). Unlike secrets, variables are plain
// configuration the LLM may see: they are NOT registered with SecretNAT and NOT
// masked in the pane echo. Storage is .rysh/variables.
//
// This file also owns the combined ${NAME} resolver used when an agent/humanoid
// frontmatter or body is constructed: it checks the secret store first, then the
// variable store, then the process environment — so a skill can reference either
// a secret or a variable by the same ${NAME} syntax.

import (
	"os"
	"strings"

	"github.com/nats-io/nats.go"
)

// newVariableStore builds the variable store over the (possibly nil) session KV
// bucket and the config-declared, workspace-scoped variables ([variables]
// section).
func newVariableStore(kv nats.KeyValue, cfgVariables map[string]map[string]string) *namedStore {
	return newNamedStore(variableKind, kv, cfgVariables)
}

// handleVariableSubcommand handles the ##variable (alias ##variables / ##var)
// system command. It delegates to the shared named-store handler wired to the
// variable store. Variables are plain config, so there is no SecretNAT sync
// (afterMut is nil) and their values are echoed verbatim.
func (w *WorkspaceActor) handleVariableSubcommand(out *strings.Builder, paneID string, parts []string) {
	w.handleNamedStoreSubcommand(out, paneID, parts, namedStoreCmd{
		store:    w.variables,
		afterMut: nil,
	})
}

// resolveNamedLayered resolves ${NAME} across the secret store and then the
// variable store — each over the scope chain's scoped tiers (session → persist →
// config, no env) — and finally the global process environment once. Secrets
// take precedence over variables when a name exists in both, and the existing
// env fallback is preserved. Returns the resolved value and whether it was found.
func (w *WorkspaceActor) resolveNamedLayered(scopes []string, name string) (string, bool) {
	if w.secrets != nil {
		if v, _, _, ok := w.secrets.getLayeredScoped(scopes, name); ok {
			return v, true
		}
	}
	if w.variables != nil {
		if v, _, _, ok := w.variables.getLayeredScoped(scopes, name); ok {
			return v, true
		}
	}
	if v, ok := os.LookupEnv(name); ok {
		return trimSecret(v), true
	}
	return "", false
}

// namedExpandLayered replaces every ${NAME} reference in in with its combined
// secret→variable→env resolution across the ordered scopes. Unresolved
// references are left verbatim so non-value ${...} text survives.
func (w *WorkspaceActor) namedExpandLayered(scopes []string, in string) string {
	return secretRefPattern.ReplaceAllStringFunc(in, func(match string) string {
		name := secretRefPattern.FindStringSubmatch(match)[1]
		if v, ok := w.resolveNamedLayered(scopes, name); ok {
			return v
		}
		return match
	})
}

// namedExpandFunc returns the combined secret+variable expander bound to the
// scope chain for paneID (tab → workspace → env), suitable for threading into the
// agent/humanoid skill-file parsers. This is the single integration point that
// makes frontmatter, system-prompt bodies, and humanoid contact configs resolve
// ${NAME} from both .rysh/secrets and .rysh/variables.
func (w *WorkspaceActor) namedExpandFunc(paneID string) func(string) string {
	scopes := w.secretScopeChain(paneID)
	return func(in string) string { return w.namedExpandLayered(scopes, in) }
}
