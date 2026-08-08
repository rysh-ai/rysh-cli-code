package actors

// model_scope.go — the LLM model inheritance hierarchy.
//
//	session > workspace > tab > lane > stack > pane
//
// A model bound at a NARROWER scope wins over anything above it: a pane's own
// selection beats its stack's, a stack's beats its lane's, and so on up to the
// session default. Nothing is copied downward — each pane resolves the nearest
// binding above it, so re-binding a lane instantly re-aims every pane under it
// that has not made a narrower choice of its own.
//
// State deliberately lives where it belongs rather than in one table:
//
//   - session   — agSetup.SessionLLM (the per-call provider decorator), because
//     it must also reach agents/humanoids that have no pane. Set by ##llm use.
//   - workspace, tab, lane, stack — w.modelBindings in this actor, keyed by
//     modelScopeKey. These have no provider seam of their own, so they are
//     resolved per pane and pushed into that pane's override holder.
//   - pane — the PaneActor's own persisted override (see pane_provider.go).
//
// The push carries the winning scope name so the pane can tell an INHERITED
// selection (recomputed by this actor, never persisted) from its OWN
// (persisted in its KV record). That distinction is what keeps a lane-wide
// re-bind from silently clobbering a pane that chose for itself.

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/llms"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// modelScope names one level of the hierarchy.
type modelScope string

const (
	scopeSession   modelScope = "session"
	scopeWorkspace modelScope = "workspace"
	scopeTab       modelScope = "tab"
	scopeLane      modelScope = "lane"
	scopeStack     modelScope = "stack"
	scopePane      modelScope = "pane"
)

// modelScopeOrder is the hierarchy broadest-first. Resolution walks it in
// reverse, so the first hit is the narrowest binding.
var modelScopeOrder = []modelScope{scopeSession, scopeWorkspace, scopeTab, scopeLane, scopeStack, scopePane}

// inheritedScopeOrder is the slice this actor resolves on a pane's behalf,
// narrowest-first: below it the pane owns its selection, above it the session
// decorator already applies the default to every provider call.
var inheritedScopeOrder = []modelScope{scopeStack, scopeLane, scopeTab, scopeWorkspace}

// modelBinding is one scope's selection, already resolved against the
// .rysh/llms registry.
type modelBinding struct {
	Ref      string // "<provider>/<name>" as declared in the registry
	Provider string // rysh provider family serving it
	Model    string // provider API model id
	Effort   string // optional reasoning effort; only the session seam applies it
}

// Empty reports whether the binding selects nothing (the scope is unbound).
func (b modelBinding) Empty() bool { return b.Model == "" }

// Label renders the binding for status output.
func (b modelBinding) Label() string {
	if b.Empty() {
		return "(unset)"
	}
	if b.Effort != "" {
		return fmt.Sprintf("%s → %s (effort %s)", b.Ref, b.Model, b.Effort)
	}
	return fmt.Sprintf("%s → %s", b.Ref, b.Model)
}

// modelScopeKey identifies one binding slot. The session scope is a singleton
// (its state lives in agSetup.SessionLLM), as is the workspace scope within
// one WorkspaceActor; tab/lane/stack are keyed by their entity id.
func modelScopeKey(scope modelScope, id string) string {
	if id == "" {
		return string(scope)
	}
	return string(scope) + ":" + id
}

// parseModelScope maps a user-typed scope word to a scope, accepting the same
// aliases the surrounding commands do (##ws, ##pg).
func parseModelScope(word string) (modelScope, bool) {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "session":
		return scopeSession, true
	case "workspace", "ws":
		return scopeWorkspace, true
	case "tab":
		return scopeTab, true
	case "lane":
		return scopeLane, true
	case "stack", "pg", "panegroup", "group":
		return scopeStack, true
	case "pane":
		return scopePane, true
	}
	return "", false
}

// paneCoords locates one pane in the hierarchy — everything needed to resolve
// which scope binds its model.
type paneCoords struct {
	TabID   string
	LaneID  string
	GroupID string
	PaneID  string
}

// paneCoordsInTab flattens one tab snapshot into the coordinates of every pane
// it holds. Pure, so scope resolution is testable without NATS.
func paneCoordsInTab(tabID string, snap *domain.TabSnapshot) []paneCoords {
	if snap == nil {
		return nil
	}
	var out []paneCoords
	for s := range domain.PaneSitesInTab(snap) {
		out = append(out, paneCoords{TabID: tabID, LaneID: s.Lane.ID, GroupID: s.Group.ID, PaneID: s.Pane.ID})
	}
	return out
}

// resolveInheritedModel returns the narrowest binding at or above the stack
// scope for one pane position, and the scope it came from. An empty binding
// with an empty scope means nothing between workspace and stack binds a model,
// so the pane falls through to its own selection or the session default.
func (w *WorkspaceActor) resolveInheritedModel(c paneCoords) (modelBinding, modelScope) {
	for _, scope := range inheritedScopeOrder {
		id := ""
		switch scope {
		case scopeStack:
			id = c.GroupID
		case scopeLane:
			id = c.LaneID
		case scopeTab:
			id = c.TabID
		case scopeWorkspace:
			id = w.workspaceName
		}
		if b, ok := w.modelBindings[modelScopeKey(scope, id)]; ok && !b.Empty() {
			return b, scope
		}
	}
	return modelBinding{}, ""
}

// setModelBinding installs (or, with an empty binding, removes) one scope's
// selection. Session is handled by the caller through agSetup.SessionLLM and
// never reaches this table.
func (w *WorkspaceActor) setModelBinding(scope modelScope, id string, b modelBinding) {
	if w.modelBindings == nil {
		w.modelBindings = map[string]modelBinding{}
	}
	key := modelScopeKey(scope, id)
	if b.Empty() {
		delete(w.modelBindings, key)
		return
	}
	w.modelBindings[key] = b
}

// modelBindingAt reads one scope's selection back.
func (w *WorkspaceActor) modelBindingAt(scope modelScope, id string) (modelBinding, bool) {
	b, ok := w.modelBindings[modelScopeKey(scope, id)]
	return b, ok
}

// resolveModelRef looks a "<provider>/<name>" reference up in .rysh/llms and
// returns the binding it activates. Every rejection is rendered to out and
// signalled by ok=false, so callers read as `if !ok { return }`.
//
// The gate here is deliberately wider than ##llm use's: a scope below the
// session binds a WHOLE provider for the panes under it, so any family rysh
// has an adapter for is selectable. ##llm use only re-pins a model id on the
// session's existing provider, which is why it is Anthropic-only.
func (w *WorkspaceActor) resolveModelRef(out *strings.Builder, ref string) (modelBinding, bool) {
	providerName, modelName, err := llms.ParseRef(ref)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] %v\n", err)
		return modelBinding{}, false
	}
	store := llms.NewStore(w.cfg.RyshDir)
	if err := store.SeedIfEmpty(); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[rysh] registry error at %s: %v\n", store.Dir(), err)
		return modelBinding{}, false
	}
	spec, err := store.Get(providerName, modelName)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "\n[rysh] %s not found in .rysh/llms — declare it with: ##llm add %s [api-model-id]\n", ref, ref)
			w.failRysh("%s not found in .rysh/llms — declare it with: ##llm add %s [api-model-id]", ref, ref)
			return modelBinding{}, false
		}
		fmt.Fprintf(out, "\n[rysh] cannot read %s: %v\n", ref, err)
		w.failRysh("cannot read %s: %v", ref, err)
		return modelBinding{}, false
	}
	if spec.Disabled {
		fmt.Fprintf(out, "\n[rysh] %s is disabled — re-enable it first: ##llm enable %s\n", ref, ref)
		w.failRysh("%s is disabled — re-enable it with ##llm enable", ref)
		return modelBinding{}, false
	}
	family := providerFamily(providerName)
	if !provider.IsKnownProviderName(family) {
		fmt.Fprintf(out, "\n[rysh] provider %q has no rysh adapter — valid: %s\n",
			providerName, strings.Join(provider.KnownProviderNames(), ", "))
		w.failRysh("provider %q has no rysh adapter", providerName)
		return modelBinding{}, false
	}
	return modelBinding{
		Ref:      providerName + "/" + spec.Name,
		Provider: family,
		Model:    spec.Model,
		Effort:   spec.Effort,
	}, true
}

// warnMissingKey calls out an absent API key at selection time rather than
// letting the next prompt 401. It mirrors the lookup the provider is actually
// built with — ##secret store first, environment last — at every scope: the
// session seat resolves here, and the pane/humanoid seats resolve through the
// same store via the resolver this workspace hands them. Presence only; the key
// is never echoed.
//
// The chain used here is the active tab's. A binding that outlives this tab (a
// workspace-wide one, say) is still resolved per pane against the pane's OWN
// tab at install time, so a tab-scoped key can make this warning pessimistic
// for panes elsewhere. Warning only — nothing is decided on it.
func (w *WorkspaceActor) warnMissingKey(out *strings.Builder, family string) {
	keyName, missing := w.missingProviderKey(family)
	if !missing {
		return
	}
	fmt.Fprintf(out, "[rysh] warning: no API key found for %s — register it with `##secret new %s <value>` or set %s in the daemon environment; prompts will fail until it is set\n",
		family, keyName, providerKeyEnvHint(family))
}

// boundScopeKeys returns the currently bound workspace/tab/lane/stack keys in
// hierarchy order — the input to the `##llm scopes` overview.
func (w *WorkspaceActor) boundScopeKeys() []string {
	keys := make([]string, 0, len(w.modelBindings))
	for k := range w.modelBindings {
		keys = append(keys, k)
	}
	rank := map[string]int{}
	for i, s := range modelScopeOrder {
		rank[string(s)] = i
	}
	sort.Slice(keys, func(i, j int) bool {
		si := rank[strings.SplitN(keys[i], ":", 2)[0]]
		sj := rank[strings.SplitN(keys[j], ":", 2)[0]]
		if si != sj {
			return si < sj
		}
		return keys[i] < keys[j]
	})
	return keys
}
