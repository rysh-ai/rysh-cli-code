// SPDX-License-Identifier: Apache-2.0

package actors

// workspace_scope_model.go — one `model` subcommand shared by every level of
// the hierarchy (session > workspace > tab > lane > stack > pane):
//
//	##session model [<provider>/<name>|list|status|default]
//	##workspace model ...        (alias: ##ws model)
//	##tab model ...
//	##lane model ...
//	##stack model ...            (aliases: ##pg model, ##panegroup model)
//	##pane model ...
//
// All six read identically and all resolve refs against the same .rysh/llms
// registry ##llm lists. Where they differ is the seam each one drives:
//
//   - session binds agSetup.SessionLLM, the per-call provider decorator, so it
//     also reaches agents and humanoids that have no pane. It re-pins a model
//     id on the session's existing provider, hence Anthropic-only — the same
//     gate ##llm use has always had.
//   - workspace/tab/lane/stack have no seam of their own. They record a
//     binding here, and every pane underneath resolves the nearest one and
//     receives it as an INHERITED selection in its override holder.
//   - pane sets the pane's OWN selection, which outranks all of the above.
//
// Because the lower scopes build a whole provider per pane rather than
// re-pinning a model id, they accept any provider family rysh has an adapter
// for — openai, gemini, ollama, claude-cli — not just anthropic.

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/llms"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handleScopeModelCommand is the entry point for every `##<scope> model …`
// invocation. Sends are performed here (guarded on w.pub) for the scopes that
// fan out; the pane scope keeps returning its message to its caller.
func (w *WorkspaceActor) handleScopeModelCommand(out *strings.Builder, paneID string, scope modelScope, args []string) {
	if scope == scopePane {
		if m := w.cmdPaneModel(out, paneID, args); m != nil && w.pub != nil {
			_ = w.pub.Send(msg.T("pane", paneID, "inbox"), m)
		}
		return
	}

	action := ""
	if len(args) > 0 {
		action = strings.TrimSpace(args[0])
	}
	switch strings.ToLower(action) {
	case "":
		w.cmdScopeModelStatus(out, paneID, scope)
		return
	case "status", "info":
		w.cmdScopeModelStatus(out, paneID, scope)
		return
	case "list":
		store := llms.NewStore(w.cfg.RyshDir)
		if err := store.SeedIfEmpty(); err != nil {
			w.failRysh("%v", err)
			fmt.Fprintf(out, "\n[rysh] registry error at %s: %v\n", store.Dir(), err)
			return
		}
		w.cmdLLMList(out, store)
		return
	case "scopes", "tree":
		w.cmdModelScopes(out, paneID)
		return
	}

	// Resolve which entity this scope refers to before doing anything else: a
	// tab/lane/stack binding is meaningless without one.
	id, label, ok := w.scopeEntity(out, paneID, scope)
	if !ok {
		return
	}

	switch strings.ToLower(action) {
	case "default", "clear", "reset":
		w.cmdScopeModelClear(out, scope, id, label)
	default:
		if !strings.Contains(action, "/") {
			ryshWriter(out).UsageLine(fmt.Sprintf("##%s model <provider>/<name>   (also: list, status, default)", scope))
			w.failRyshUsage("usage: %s", fmt.Sprintf("##%s model <provider>/<name>   (also: list, status, default)", scope))
			return
		}
		w.cmdScopeModelSet(out, scope, id, label, action)
	}
}

// scopeEntity resolves the id and display label of the entity a scope command
// addresses: the active tab/lane/stack, or the workspace itself.
func (w *WorkspaceActor) scopeEntity(out *strings.Builder, paneID string, scope modelScope) (id, label string, ok bool) {
	switch scope {
	case scopeSession:
		return "", w.currentSessionName(), true
	case scopeWorkspace:
		return w.workspaceName, w.workspaceName, true
	}
	c, found := w.findPaneCoords(paneID)
	if !found {
		fmt.Fprintf(out, "\n[rysh] cannot resolve the active %s (no active pane, or its tab snapshot is unavailable)\n", scope)
		w.failRysh("cannot resolve the active %s (no active pane, or its tab snapshot is unavailable)", scope)
		return "", "", false
	}
	switch scope {
	case scopeTab:
		return c.TabID, c.TabID, true
	case scopeLane:
		return c.LaneID, c.LaneID, true
	case scopeStack:
		return c.GroupID, c.GroupID, true
	}
	return "", "", false
}

// cmdScopeModelSet binds a model at one scope and re-aims every pane under it
// that has not chosen for itself.
func (w *WorkspaceActor) cmdScopeModelSet(out *strings.Builder, scope modelScope, id, label, ref string) {
	if scope == scopeSession {
		// One session mechanism, not two: delegate to the ##llm use path that
		// owns agSetup.SessionLLM.
		store := llms.NewStore(w.cfg.RyshDir)
		if err := store.SeedIfEmpty(); err != nil {
			w.failRysh("%v", err)
			fmt.Fprintf(out, "\n[rysh] registry error at %s: %v\n", store.Dir(), err)
			return
		}
		w.cmdLLMUse(out, store, ref)
		return
	}
	b, ok := w.resolveModelRef(out, ref)
	if !ok {
		return
	}
	w.setModelBinding(scope, id, b)
	fmt.Fprintf(out, "\n[rysh] %s model set: %s (%s %s)\n", scope, b.Label(), scope, label)
	if b.Effort != "" {
		fmt.Fprintf(out, "[rysh] note: effort %q applies at session scope only (##llm use) — the %s binding pins the model\n",
			b.Effort, scope)
	}
	w.warnMissingKey(out, b.Provider)
	w.reportModelFanout(out, scope)
}

// cmdScopeModelClear drops one scope's binding; panes under it fall back to
// the next scope up.
func (w *WorkspaceActor) cmdScopeModelClear(out *strings.Builder, scope modelScope, id, label string) {
	if scope == scopeSession {
		if w.agSetup != nil && w.agSetup.SessionLLM != nil {
			w.agSetup.SessionLLM.Set("", "")
		}
		w.sessionLLMRef = ""
		fmt.Fprintf(out, "\n[rysh] session model cleared — back to the config default (%s)\n", w.configuredModelLabel())
		return
	}
	if _, bound := w.modelBindingAt(scope, id); !bound {
		fmt.Fprintf(out, "\n[rysh] %s %s binds no model — nothing to clear\n", scope, label)
		return
	}
	w.setModelBinding(scope, id, modelBinding{})
	fmt.Fprintf(out, "\n[rysh] %s model cleared (%s %s) — panes fall back to the next bound scope above\n", scope, scope, label)
	w.reportModelFanout(out, scope)
}

// reportModelFanout re-resolves every pane and says how many actually changed,
// so a bind at a scope with no panes under it does not read as a success.
func (w *WorkspaceActor) reportModelFanout(out *strings.Builder, scope modelScope) {
	changed := w.applyInheritedModels()
	switch changed {
	case 0:
		fmt.Fprintf(out, "[rysh] no pane changed model: every pane under this %s already resolves elsewhere (narrower binding or its own)\n", scope)
	case 1:
		fmt.Fprintf(out, "[rysh] 1 pane re-aimed; applies to its next agentic prompt\n")
	default:
		fmt.Fprintf(out, "[rysh] %d panes re-aimed; applies to their next agentic prompt\n", changed)
	}
}

// inheritedPush is one pane's changed inherited selection, ready to send.
type inheritedPush struct {
	PaneID   string
	Provider string
	Model    string
	Scope    string
}

// inheritedFingerprint encodes what a pane currently inherits. A pane that
// inherits nothing fingerprints as "" — the zero value of paneInheritedModel —
// so panes no binding reaches compare equal to panes never seen, and are
// neither pushed nor counted as re-aimed.
func inheritedFingerprint(b modelBinding, scope modelScope) string {
	if b.Empty() {
		return ""
	}
	return string(scope) + "|" + b.Provider + "|" + b.Model
}

// planInheritedPushes re-resolves every pane position and returns only the ones
// whose inherited selection actually moved, updating the fingerprint table as
// it goes. Split out from the sending so the "only what changed" rule is
// testable without NATS.
func (w *WorkspaceActor) planInheritedPushes(coords []paneCoords) []inheritedPush {
	if w.paneInheritedModel == nil {
		w.paneInheritedModel = map[string]string{}
	}
	var pushes []inheritedPush
	seen := make(map[string]bool, len(coords))
	for _, c := range coords {
		seen[c.PaneID] = true
		b, scope := w.resolveInheritedModel(c)
		fingerprint := inheritedFingerprint(b, scope)
		if w.paneInheritedModel[c.PaneID] == fingerprint {
			continue
		}
		w.paneInheritedModel[c.PaneID] = fingerprint
		pushes = append(pushes, inheritedPush{
			PaneID:   c.PaneID,
			Provider: b.Provider,
			Model:    b.Model,
			// An empty binding still needs a non-pane scope so the pane clears
			// its INHERITED slot and not its own selection.
			Scope: scopeOrInherited(scope),
		})
	}
	// Forget panes that no longer exist, so a recycled id cannot inherit a
	// stale fingerprint and skip its push.
	for id := range w.paneInheritedModel {
		if !seen[id] {
			delete(w.paneInheritedModel, id)
		}
	}
	return pushes
}

// applyInheritedModels re-resolves the inherited selection for every pane in
// every tab and pushes the ones that changed. Returns the number pushed.
//
// Panes are pushed, never polled: the resolution lives here because only this
// actor sees the whole tree, and the pane keeps the result in its inherited
// slot so its own selection stays untouched.
func (w *WorkspaceActor) applyInheritedModels() int {
	var coords []paneCoords
	for _, tab := range w.tabs {
		if snap := w.queryTabSnapshot(tab.id); snap != nil {
			coords = append(coords, paneCoordsInTab(tab.id, snap)...)
		}
	}
	pushes := w.planInheritedPushes(coords)
	for _, p := range pushes {
		if w.pub == nil {
			break
		}
		_ = w.pub.Send(msg.T("pane", p.PaneID, "inbox"), &msg.MsgPaneSetProvider{
			Provider: p.Provider,
			Model:    p.Model,
			Scope:    p.Scope,
		})
	}
	return len(pushes)
}

// scopeOrInherited keeps clear-pushes addressed at the inherited slot: an
// unresolved scope would serialize as "" and the pane would read it as its own.
func scopeOrInherited(scope modelScope) string {
	if scope == "" {
		return string(scopeWorkspace)
	}
	return string(scope)
}

// cmdScopeModelStatus answers "what does this scope bind, and what wins here?"
func (w *WorkspaceActor) cmdScopeModelStatus(out *strings.Builder, paneID string, scope modelScope) {
	id, label, ok := w.scopeEntity(out, paneID, scope)
	if !ok {
		return
	}
	fmt.Fprintf(out, "\n[rysh] %s model (%s)\n", scope, label)
	if scope == scopeSession {
		if w.sessionLLMRef != "" {
			fmt.Fprintf(out, "  bound     : %s (##llm clear to reset)\n", w.sessionLLMRef)
		} else {
			fmt.Fprintf(out, "  bound     : (unset) — config default %s\n", w.configuredModelLabel())
		}
		fmt.Fprintf(out, "  set with  : ##session model <provider>/<name>   (same seam as ##llm use)\n")
		return
	}
	if b, bound := w.modelBindingAt(scope, id); bound {
		fmt.Fprintf(out, "  bound     : %s\n", b.Label())
		fmt.Fprintf(out, "  provider  : %s\n", b.Provider)
		fmt.Fprintf(out, "  clear with: ##%s model default\n", scope)
	} else {
		fmt.Fprintf(out, "  bound     : (unset) — inherits from the nearest bound scope above\n")
		fmt.Fprintf(out, "  set with  : ##%s model <provider>/<name>   (see ##%s model list)\n", scope, scope)
	}
	if c, found := w.findPaneCoords(paneID); found {
		eff, from := w.resolveInheritedModel(c)
		if eff.Empty() {
			fmt.Fprintf(out, "  active pane resolves to: %s (session)\n", w.sessionModelLabel())
		} else {
			fmt.Fprintf(out, "  active pane resolves to: %s (from %s)\n", eff.Label(), from)
		}
	}
}

// cmdModelScopes renders the whole hierarchy for the active pane — every level
// with what it binds, and which one actually wins. This is the view that makes
// the precedence rule legible instead of something to reason about.
func (w *WorkspaceActor) cmdModelScopes(out *strings.Builder, paneID string) {
	fmt.Fprintf(out, "\n[rysh] model scopes (narrower wins)\n")
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 66))

	c, found := w.findPaneCoords(paneID)
	paneSnap := w.findPaneSnapshot(paneID)

	// Walk broadest→narrowest so the printed order matches the hierarchy, and
	// mark the narrowest bound level as the winner.
	type row struct {
		scope modelScope
		label string
		bound string
	}
	rows := []row{{scopeSession, w.currentSessionName(), ""}}
	if w.sessionLLMRef != "" {
		rows[0].bound = w.sessionLLMRef
	}
	add := func(scope modelScope, id, label string) {
		r := row{scope: scope, label: label}
		if b, ok := w.modelBindingAt(scope, id); ok {
			r.bound = b.Label()
		}
		rows = append(rows, r)
	}
	add(scopeWorkspace, w.workspaceName, w.workspaceName)
	if found {
		add(scopeTab, c.TabID, c.TabID)
		add(scopeLane, c.LaneID, c.LaneID)
		add(scopeStack, c.GroupID, c.GroupID)
		paneRow := row{scope: scopePane, label: c.PaneID}
		if paneSnap != nil && paneSnap.ProviderOverrideModel != "" {
			paneRow.bound = paneSnap.ProviderOverrideModel
		}
		rows = append(rows, paneRow)
	}

	winner := -1
	for i := range rows {
		if rows[i].bound != "" {
			winner = i
		}
	}
	for i, r := range rows {
		marker := "  "
		if i == winner {
			marker = "> "
		}
		bound := r.bound
		if bound == "" {
			bound = "-"
		}
		fmt.Fprintf(out, "%s%-10s %-24s %s\n", marker, r.scope, truncateScopeLabel(r.label), bound)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 66))
	if winner < 0 {
		fmt.Fprintf(out, "nothing bound — every pane runs the config default (%s)\n", w.configuredModelLabel())
	} else {
		fmt.Fprintf(out, "in effect for the active pane: %s (%s)\n", rows[winner].bound, rows[winner].scope)
	}
	fmt.Fprintf(out, "bind at any level: ##<session|workspace|tab|lane|stack|pane> model <provider>/<name>\n")
	if !found {
		fmt.Fprintf(out, "note: tab/lane/stack/pane rows need an active pane with a reachable tab snapshot\n")
	}

	// Bindings elsewhere in the tree are invisible in the chain above but still
	// govern their own panes — list them so the tree is not mistaken for the
	// full picture.
	var elsewhere []string
	onPath := map[string]bool{}
	if found {
		onPath[modelScopeKey(scopeTab, c.TabID)] = true
		onPath[modelScopeKey(scopeLane, c.LaneID)] = true
		onPath[modelScopeKey(scopeStack, c.GroupID)] = true
	}
	onPath[modelScopeKey(scopeWorkspace, w.workspaceName)] = true
	for _, key := range w.boundScopeKeys() {
		if !onPath[key] {
			elsewhere = append(elsewhere, fmt.Sprintf("%s = %s", key, w.modelBindings[key].Label()))
		}
	}
	if len(elsewhere) > 0 {
		fmt.Fprintf(out, "\nbound elsewhere (not on this pane's path):\n")
		for _, line := range elsewhere {
			fmt.Fprintf(out, "  %s\n", line)
		}
	}
}

func truncateScopeLabel(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) <= 24 {
		return s
	}
	return s[:21] + "..."
}

// findPaneCoords locates a pane in the hierarchy (tab/lane/stack ids). The
// active tab is checked first, then the rest, so a pane in a background tab
// still resolves.
func (w *WorkspaceActor) findPaneCoords(paneID string) (paneCoords, bool) {
	if paneID == "" {
		return paneCoords{}, false
	}
	order := make([]*tabInfo, 0, len(w.tabs))
	if t := w.currentTab(); t != nil {
		order = append(order, t)
	}
	for _, t := range w.tabs {
		if len(order) > 0 && t == order[0] {
			continue
		}
		order = append(order, t)
	}
	for _, t := range order {
		snap := w.queryTabSnapshot(t.id)
		if snap == nil {
			continue
		}
		for _, c := range paneCoordsInTab(t.id, snap) {
			if c.PaneID == paneID {
				return c, true
			}
		}
	}
	return paneCoords{}, false
}

// ---------------------------------------------------------------------------
// Pane scope
// ---------------------------------------------------------------------------

// cmdPaneModel handles `##pane model` and returns the message to send to the
// active pane's inbox, or nil when nothing should be sent (show / list /
// usage / validation paths). The caller owns the publish, mirroring
// cmdPaneProvider, so this handler stays unit-testable without NATS.
func (w *WorkspaceActor) cmdPaneModel(out *strings.Builder, paneID string, args []string) *msg.MsgPaneSetProvider {
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		w.failRysh("no active pane")
		return nil
	}
	if len(args) == 0 {
		fmt.Fprint(out, paneModelStatus(w.findPaneSnapshot(paneID), w.paneInheritedLabel(paneID)))
		return nil
	}

	ref := strings.TrimSpace(args[0])
	switch strings.ToLower(ref) {
	case "status", "info":
		fmt.Fprint(out, paneModelStatus(w.findPaneSnapshot(paneID), w.paneInheritedLabel(paneID)))
		return nil
	case "default", "clear", "reset":
		// Provider and model share one own-slot on the pane, so clearing the
		// model necessarily clears a `##pane provider` selection too. Say so
		// rather than let the provider silently snap back.
		fmt.Fprintf(out, "\n[rysh] pane provider+model override cleared — back to %s\n", w.paneInheritedLabel(paneID))
		fmt.Fprintf(out, "[rysh] applies to the next agentic prompt in this pane\n")
		return &msg.MsgPaneSetProvider{Scope: string(scopePane)}
	case "scopes", "tree":
		w.cmdModelScopes(out, paneID)
		return nil
	case "list":
		store := llms.NewStore(w.cfg.RyshDir)
		if err := store.SeedIfEmpty(); err != nil {
			w.failRysh("%v", err)
			fmt.Fprintf(out, "\n[rysh] registry error at %s: %v\n", store.Dir(), err)
			return nil
		}
		w.cmdLLMList(out, store)
		return nil
	}

	if !strings.Contains(ref, "/") {
		ryshWriter(out).UsageLine("##pane model <provider>/<name>   (also: list, scopes, status, default)")
		w.failRyshUsage("usage: %s", "##pane model <provider>/<name>   (also: list, scopes, status, default)")
		return nil
	}
	b, ok := w.resolveModelRef(out, ref)
	if !ok {
		return nil
	}
	fmt.Fprintf(out, "\n[rysh] pane model set to %s (provider %s)\n", b.Label(), b.Provider)
	fmt.Fprintf(out, "[rysh] applies to the next agentic prompt in this pane (no respawn needed); persisted with the pane across detach/attach\n")
	fmt.Fprintf(out, "[rysh] a pane binding outranks its stack, lane, tab, workspace and the session\n")
	w.warnMissingKey(out, b.Provider)
	return &msg.MsgPaneSetProvider{Provider: b.Provider, Model: b.Model, Scope: string(scopePane)}
}

// paneInheritedLabel names what a pane falls back to with no own selection:
// the nearest bound scope above it, else the session default.
func (w *WorkspaceActor) paneInheritedLabel(paneID string) string {
	if c, ok := w.findPaneCoords(paneID); ok {
		if b, scope := w.resolveInheritedModel(c); !b.Empty() {
			return fmt.Sprintf("%s (from %s)", b.Label(), scope)
		}
	}
	return w.sessionModelLabel()
}

// sessionModelLabel names the session-scope model: the ##llm binding when one
// is set, otherwise the configured default.
func (w *WorkspaceActor) sessionModelLabel() string {
	if w.sessionLLMRef != "" {
		return w.sessionLLMRef + " (##llm session default)"
	}
	return w.configuredModelLabel()
}

// paneModelStatus renders the no-args view: the model that will serve this
// pane's next agentic prompt, and where it comes from.
func paneModelStatus(snap *domain.PaneSnapshot, inherited string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[rysh] pane model\n")
	if snap == nil {
		fmt.Fprintf(&b, "  effective : %s (pane state unavailable)\n", inherited)
		return b.String()
	}
	if snap.ProviderOverrideModel == "" {
		fmt.Fprintf(&b, "  effective : %s\n", inherited)
		fmt.Fprintf(&b, "  override  : none (set with ##pane model <provider>/<name>)\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  effective : %s\n", snap.ProviderOverrideModel)
	fmt.Fprintf(&b, "  provider  : %s\n", snap.ProviderName)
	fmt.Fprintf(&b, "  inherited : %s (restore with ##pane model default)\n", inherited)
	return b.String()
}
