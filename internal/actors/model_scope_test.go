// SPDX-License-Identifier: Apache-2.0

package actors

// Tests for the model inheritance hierarchy
// (session > workspace > tab > lane > stack > pane; see model_scope.go).
//
// Resolution is pure — it reads the binding table and a pane's coordinates —
// so it is exercised directly, without NATS or a live tab tree.

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

func bindingFor(model string) modelBinding {
	return modelBinding{Ref: "anthropic/" + model, Provider: "anthropic", Model: model}
}

func newScopeWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	return &WorkspaceActor{
		cfg:           config.Config{ProviderName: "claude", RyshDir: t.TempDir()},
		workspaceName: "ws1",
		sessionName:   "sess1",
		modelBindings: map[string]modelBinding{},
	}
}

var testCoords = paneCoords{TabID: "t1", LaneID: "l1", GroupID: "g1", PaneID: "p1"}

// TestResolveInheritedModel_NarrowerWins is the core precedence rule: each
// level added below the previous one takes over.
func TestResolveInheritedModel_NarrowerWins(t *testing.T) {
	w := newScopeWorkspace(t)

	// Nothing bound → nothing inherited; the pane falls through to the session.
	if b, scope := w.resolveInheritedModel(testCoords); !b.Empty() || scope != "" {
		t.Fatalf("unbound = %v/%q, want empty", b, scope)
	}

	for _, step := range []struct {
		scope modelScope
		id    string
		model string
	}{
		{scopeWorkspace, "ws1", "ws-model"},
		{scopeTab, "t1", "tab-model"},
		{scopeLane, "l1", "lane-model"},
		{scopeStack, "g1", "stack-model"},
	} {
		w.setModelBinding(step.scope, step.id, bindingFor(step.model))
		b, scope := w.resolveInheritedModel(testCoords)
		if b.Model != step.model || scope != step.scope {
			t.Fatalf("after binding %s: got %s/%s, want %s/%s", step.scope, b.Model, scope, step.model, step.scope)
		}
	}

	// Unbinding walks precedence back up, level by level.
	for _, step := range []struct {
		scope modelScope
		id    string
		want  string
	}{
		{scopeStack, "g1", "lane-model"},
		{scopeLane, "l1", "tab-model"},
		{scopeTab, "t1", "ws-model"},
	} {
		w.setModelBinding(step.scope, step.id, modelBinding{})
		if b, _ := w.resolveInheritedModel(testCoords); b.Model != step.want {
			t.Fatalf("after clearing %s: got %s, want %s", step.scope, b.Model, step.want)
		}
	}
	w.setModelBinding(scopeWorkspace, "ws1", modelBinding{})
	if b, _ := w.resolveInheritedModel(testCoords); !b.Empty() {
		t.Fatalf("fully cleared = %v, want empty", b)
	}
}

// TestResolveInheritedModel_SiblingsUnaffected: a binding is scoped to ONE
// entity id, so a sibling lane/stack must not pick it up.
func TestResolveInheritedModel_SiblingsUnaffected(t *testing.T) {
	w := newScopeWorkspace(t)
	w.setModelBinding(scopeLane, "l1", bindingFor("lane-one"))

	if b, _ := w.resolveInheritedModel(testCoords); b.Model != "lane-one" {
		t.Fatalf("lane l1 pane = %q", b.Model)
	}
	sibling := paneCoords{TabID: "t1", LaneID: "l2", GroupID: "g9", PaneID: "p9"}
	if b, _ := w.resolveInheritedModel(sibling); !b.Empty() {
		t.Fatalf("lane l2 pane inherited %q from lane l1", b.Model)
	}

	// The tab above both is shared, so it reaches the sibling — and loses to
	// the nearer lane binding on the original.
	w.setModelBinding(scopeTab, "t1", bindingFor("tab-model"))
	if b, scope := w.resolveInheritedModel(sibling); b.Model != "tab-model" || scope != scopeTab {
		t.Fatalf("sibling = %s/%s, want tab-model/tab", b.Model, scope)
	}
	if b, scope := w.resolveInheritedModel(testCoords); b.Model != "lane-one" || scope != scopeLane {
		t.Fatalf("bound lane = %s/%s, want lane-one/lane", b.Model, scope)
	}
}

func TestPaneCoordsInTab(t *testing.T) {
	snap := &domain.TabSnapshot{Lanes: []domain.LaneSnapshot{
		{ID: "l1", PaneGroups: []domain.PaneGroupSnapshot{
			{ID: "g1", Panes: []domain.PaneSnapshot{{ID: "p1"}, {ID: "p2"}}},
			{ID: "g2", Panes: []domain.PaneSnapshot{{ID: "p3"}}},
		}},
		{ID: "l2", PaneGroups: []domain.PaneGroupSnapshot{
			{ID: "g3", Panes: []domain.PaneSnapshot{{ID: "p4"}}},
		}},
	}}
	got := paneCoordsInTab("t1", snap)
	if len(got) != 4 {
		t.Fatalf("got %d pane coords, want 4: %+v", len(got), got)
	}
	if got[2] != (paneCoords{TabID: "t1", LaneID: "l1", GroupID: "g2", PaneID: "p3"}) {
		t.Errorf("third coord = %+v", got[2])
	}
	if got[3].LaneID != "l2" || got[3].GroupID != "g3" {
		t.Errorf("fourth coord = %+v", got[3])
	}
	if paneCoordsInTab("t1", nil) != nil {
		t.Error("nil snapshot must yield no coords")
	}
}

func TestParseModelScope(t *testing.T) {
	for word, want := range map[string]modelScope{
		"session": scopeSession, "workspace": scopeWorkspace, "ws": scopeWorkspace,
		"tab": scopeTab, "lane": scopeLane,
		"stack": scopeStack, "pg": scopeStack, "panegroup": scopeStack,
		"pane": scopePane, "PANE": scopePane,
	} {
		if got, ok := parseModelScope(word); !ok || got != want {
			t.Errorf("parseModelScope(%q) = %q/%v, want %q", word, got, ok, want)
		}
	}
	if _, ok := parseModelScope("humanoid"); ok {
		t.Error("unknown scope word must not parse")
	}
}

// ---------------------------------------------------------------------------
// Pane side: the two slots
// ---------------------------------------------------------------------------

// TestPaneOwnSelectionBeatsInherited is the rule the user asked for from the
// bottom up: a pane that chose for itself ignores anything pushed from above.
func TestPaneOwnSelectionBeatsInherited(t *testing.T) {
	p := newProviderTestPane()

	// Inherited from the lane first.
	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{
		Provider: "anthropic", Model: "lane-model", Scope: string(scopeLane)})
	if _, model, scope := p.effectiveProviderSelection(); model != "lane-model" || scope != string(scopeLane) {
		t.Fatalf("inherited = %s/%s", model, scope)
	}
	if p.kvDirty {
		t.Error("an inherited selection must NOT mark the pane dirty — the scope recomputes it")
	}

	// The pane then chooses for itself: its own selection takes over.
	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{
		Provider: "anthropic", Model: "pane-model", Scope: string(scopePane)})
	if _, model, scope := p.effectiveProviderSelection(); model != "pane-model" || scope != string(scopePane) {
		t.Fatalf("own = %s/%s, want pane-model/pane", model, scope)
	}
	if !p.kvDirty {
		t.Error("the pane's own selection must persist")
	}

	// A later lane-wide re-bind must NOT clobber it.
	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{
		Provider: "anthropic", Model: "lane-model-2", Scope: string(scopeLane)})
	if _, model, _ := p.effectiveProviderSelection(); model != "pane-model" {
		t.Fatalf("lane re-bind clobbered the pane's own selection: %s", model)
	}

	// Clearing the pane's own selection falls back to the inherited one that
	// was quietly kept up to date underneath.
	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{Scope: string(scopePane)})
	if _, model, scope := p.effectiveProviderSelection(); model != "lane-model-2" || scope != string(scopeLane) {
		t.Fatalf("after clearing own = %s/%s, want lane-model-2/lane", model, scope)
	}
}

// TestPaneInheritedClearFallsThroughToSession: clearing the inherited slot with
// no own selection drops the holder entirely, so the pane resolves through the
// session decorator again.
func TestPaneInheritedClearFallsThroughToSession(t *testing.T) {
	p := newProviderTestPane()
	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{
		Provider: "gemini", Model: "gemini-2.5-pro", Scope: string(scopeTab)})
	if p.providerHolder.Get() == nil {
		t.Fatal("inherited selection must install a provider")
	}
	if p.providerOverride != "" {
		t.Fatalf("inherited selection leaked into the pane's own slot: %q", p.providerOverride)
	}

	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{Scope: string(scopeWorkspace)})
	if p.providerHolder.Get() != nil {
		t.Fatal("clearing the inherited slot must empty the holder")
	}
	if p.providerName != p.agSetup.Provider.Name() {
		t.Errorf("providerName = %q, want the session provider's", p.providerName)
	}
}

// TestInheritedSelectionNotPersisted is the detach/attach half of the same
// rule: only the pane's own choice rides the snapshot, because every broader
// scope re-pushes on restore.
func TestInheritedSelectionNotPersisted(t *testing.T) {
	p := newProviderTestPane()
	p.handleSetProviderOverride(&msg.MsgPaneSetProvider{
		Provider: "claude-cli", Scope: string(scopeStack)})

	snap := p.buildSnapshot(false, false, true)
	if snap.ProviderOverride != "" || snap.ProviderOverrideModel != "" {
		t.Fatalf("inherited selection was persisted: %q/%q", snap.ProviderOverride, snap.ProviderOverrideModel)
	}

	restored := newProviderTestPane()
	restored.RestoreState(snap)
	restored.applyEffectiveProvider()
	if restored.providerHolder.Get() != nil {
		t.Error("a restored pane must not resurrect an inherited selection on its own")
	}
}

// TestIsInheritedScope pins the slot routing, including the empty-scope
// back-compat case every pre-existing sender relies on.
func TestIsInheritedScope(t *testing.T) {
	for scope, want := range map[string]bool{
		"": false, "pane": false, "PANE": false, " pane ": false,
		"stack": true, "lane": true, "tab": true, "workspace": true, "session": true,
	} {
		if got := isInheritedScope(scope); got != want {
			t.Errorf("isInheritedScope(%q) = %v, want %v", scope, got, want)
		}
	}
}

// TestScopeOrInherited: an unresolved scope must still address the inherited
// slot, or a clear-push would wipe the pane's own selection.
func TestScopeOrInherited(t *testing.T) {
	if got := scopeOrInherited(""); isInheritedScope(got) != true {
		t.Errorf("scopeOrInherited(\"\") = %q, which the pane reads as its own slot", got)
	}
	if got := scopeOrInherited(scopeLane); got != "lane" {
		t.Errorf("scopeOrInherited(lane) = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Ref resolution
// ---------------------------------------------------------------------------

func TestResolveModelRef(t *testing.T) {
	w := newScopeWorkspace(t)
	var out strings.Builder

	b, ok := w.resolveModelRef(&out, "anthropic/fable5")
	if !ok {
		t.Fatalf("seeded ref rejected: %s", out.String())
	}
	if b.Ref != "anthropic/fable5" || b.Model != "claude-fable-5" || b.Provider != "anthropic" {
		t.Fatalf("binding = %+v", b)
	}

	// Cross-family is fine below the session: the scope binds a whole provider.
	out.Reset()
	if b, ok = w.resolveModelRef(&out, "openai/gpt-4o"); !ok || b.Provider != "openai" {
		t.Fatalf("openai ref = %+v, ok=%v: %s", b, ok, out.String())
	}

	// A provider with no adapter is refused with the valid list.
	out.Reset()
	if _, ok = w.resolveModelRef(&out, "grok/grok-4"); ok {
		t.Error("grok has no adapter and must be refused")
	}
	if !strings.Contains(out.String(), "no rysh adapter") {
		t.Errorf("rejection text: %s", out.String())
	}

	// Disabled entries are refused everywhere, not just at ##llm use.
	llmCmd(t, w, "disable", "anthropic/fable5")
	out.Reset()
	if _, ok = w.resolveModelRef(&out, "anthropic/fable5"); ok {
		t.Error("disabled model must be refused")
	}
	if !strings.Contains(out.String(), "##llm enable") {
		t.Errorf("refusal must name the way back: %s", out.String())
	}

	// Malformed and unknown refs.
	out.Reset()
	if _, ok = w.resolveModelRef(&out, "fable5"); ok {
		t.Error("a ref without a provider must be refused")
	}
	out.Reset()
	if _, ok = w.resolveModelRef(&out, "anthropic/nope"); ok {
		t.Error("unknown ref must be refused")
	}
	if !strings.Contains(out.String(), "##llm add anthropic/nope") {
		t.Errorf("unknown ref must point at add: %s", out.String())
	}
}

// ---------------------------------------------------------------------------
// Command surface
// ---------------------------------------------------------------------------

func scopeModelCmd(t *testing.T, w *WorkspaceActor, scope modelScope, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handleScopeModelCommand(&out, "", scope, args)
	return out.String()
}

// TestScopeModelCommand_WorkspaceBindAndClear exercises the one scope that
// needs no live tab tree to resolve its entity.
func TestScopeModelCommand_WorkspaceBindAndClear(t *testing.T) {
	w := newScopeWorkspace(t)

	out := scopeModelCmd(t, w, scopeWorkspace, "anthropic/fable5")
	if !strings.Contains(out, "workspace model set") || !strings.Contains(out, "claude-fable-5") {
		t.Fatalf("bind output:\n%s", out)
	}
	b, bound := w.modelBindingAt(scopeWorkspace, "ws1")
	if !bound || b.Model != "claude-fable-5" {
		t.Fatalf("binding table = %+v/%v", b, bound)
	}

	if out = scopeModelCmd(t, w, scopeWorkspace); !strings.Contains(out, "bound     : anthropic/fable5") {
		t.Errorf("status output:\n%s", out)
	}

	if out = scopeModelCmd(t, w, scopeWorkspace, "default"); !strings.Contains(out, "workspace model cleared") {
		t.Errorf("clear output:\n%s", out)
	}
	if _, bound = w.modelBindingAt(scopeWorkspace, "ws1"); bound {
		t.Error("clear must remove the binding")
	}

	// Clearing an unbound scope says so instead of reporting a fake success.
	if out = scopeModelCmd(t, w, scopeWorkspace, "default"); !strings.Contains(out, "nothing to clear") {
		t.Errorf("redundant clear output:\n%s", out)
	}
}

// TestScopeModelCommand_SessionDelegatesToLLM keeps one session mechanism:
// ##session model drives the same agSetup.SessionLLM seam ##llm use does.
func TestScopeModelCommand_SessionDelegatesToLLM(t *testing.T) {
	w := newScopeWorkspace(t)
	w.agSetup = &agentic.Setup{SessionLLM: provider.NewSessionDefaults()}

	out := scopeModelCmd(t, w, scopeSession, "anthropic/fable5")
	if !strings.Contains(out, "session default set") {
		t.Fatalf("session bind output:\n%s", out)
	}
	if model, _ := w.agSetup.SessionLLM.Get(); model != "claude-fable-5" {
		t.Fatalf("SessionLLM = %q, want claude-fable-5", model)
	}
	// It must NOT land in the binding table — the decorator already applies it
	// to every provider call, and double-applying would break `clear`.
	if _, bound := w.modelBindingAt(scopeSession, ""); bound {
		t.Error("session scope must not use the pane-push binding table")
	}

	if out = scopeModelCmd(t, w, scopeSession, "default"); !strings.Contains(out, "session model cleared") {
		t.Errorf("session clear output:\n%s", out)
	}
	if model, _ := w.agSetup.SessionLLM.Get(); model != "" {
		t.Errorf("SessionLLM still %q after clear", model)
	}
}

func TestScopeModelCommand_UsageAndList(t *testing.T) {
	w := newScopeWorkspace(t)

	if out := scopeModelCmd(t, w, scopeWorkspace, "fable5"); !strings.Contains(out, "usage: ##workspace model") {
		t.Errorf("bare word must report usage:\n%s", out)
	}
	if out := scopeModelCmd(t, w, scopeWorkspace, "list"); !strings.Contains(out, "fable5") {
		t.Errorf("list must render the registry:\n%s", out)
	}
	// Scopes needing a live tab tree say why they cannot resolve, rather than
	// silently binding nothing.
	for _, scope := range []modelScope{scopeTab, scopeLane, scopeStack} {
		out := scopeModelCmd(t, w, scope, "anthropic/fable5")
		if !strings.Contains(out, "cannot resolve the active "+string(scope)) {
			t.Errorf("%s without an active pane:\n%s", scope, out)
		}
	}
}

// TestBoundScopeKeysOrdering: the overview lists broadest-first so it reads as
// the hierarchy, not as map order.
func TestBoundScopeKeysOrdering(t *testing.T) {
	w := newScopeWorkspace(t)
	w.setModelBinding(scopeStack, "g1", bindingFor("m"))
	w.setModelBinding(scopeWorkspace, "ws1", bindingFor("m"))
	w.setModelBinding(scopeLane, "l1", bindingFor("m"))
	w.setModelBinding(scopeTab, "t1", bindingFor("m"))

	got := w.boundScopeKeys()
	want := []string{"workspace:ws1", "tab:t1", "lane:l1", "stack:g1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("boundScopeKeys() = %v, want %v", got, want)
		}
	}
}

func TestModelBindingLabel(t *testing.T) {
	if got := (modelBinding{}).Label(); got != "(unset)" {
		t.Errorf("empty label = %q", got)
	}
	b := modelBinding{Ref: "anthropic/fable5", Model: "claude-fable-5"}
	if got := b.Label(); got != "anthropic/fable5 → claude-fable-5" {
		t.Errorf("label = %q", got)
	}
	b.Effort = "high"
	if got := b.Label(); !strings.Contains(got, "effort high") {
		t.Errorf("label with effort = %q", got)
	}
}

// TestPlanInheritedPushes_OnlyWhatChanged: a re-bind must touch only the panes
// it actually moves, and must never fingerprint an unbound pane as changed.
func TestPlanInheritedPushes_OnlyWhatChanged(t *testing.T) {
	w := newScopeWorkspace(t)
	coords := []paneCoords{
		{TabID: "t1", LaneID: "l1", GroupID: "g1", PaneID: "p1"},
		{TabID: "t1", LaneID: "l1", GroupID: "g2", PaneID: "p2"},
		{TabID: "t1", LaneID: "l2", GroupID: "g3", PaneID: "p3"},
	}

	// Nothing bound: no pane inherits anything, so nothing is pushed. Without
	// this, every unbound pane would take a pointless clear-push on first run.
	if got := w.planInheritedPushes(coords); len(got) != 0 {
		t.Fatalf("unbound plan = %+v, want none", got)
	}

	// Bind lane l1: exactly its two panes move.
	w.setModelBinding(scopeLane, "l1", bindingFor("lane-model"))
	got := w.planInheritedPushes(coords)
	if len(got) != 2 {
		t.Fatalf("lane bind moved %d panes, want 2: %+v", len(got), got)
	}
	for _, p := range got {
		if p.PaneID == "p3" {
			t.Errorf("pane in lane l2 must not be touched by a lane l1 bind")
		}
		if p.Model != "lane-model" || p.Scope != "lane" {
			t.Errorf("push = %+v", p)
		}
	}

	// Re-running with no change is a no-op.
	if got = w.planInheritedPushes(coords); len(got) != 0 {
		t.Fatalf("idempotent re-run moved %+v", got)
	}

	// Binding the tab above changes only the lane-l2 pane; l1's two keep the
	// nearer lane binding.
	w.setModelBinding(scopeTab, "t1", bindingFor("tab-model"))
	got = w.planInheritedPushes(coords)
	if len(got) != 1 || got[0].PaneID != "p3" || got[0].Model != "tab-model" {
		t.Fatalf("tab bind plan = %+v, want only p3 → tab-model", got)
	}

	// Clearing everything sends each affected pane a clear addressed at the
	// INHERITED slot — never at its own.
	w.setModelBinding(scopeLane, "l1", modelBinding{})
	w.setModelBinding(scopeTab, "t1", modelBinding{})
	got = w.planInheritedPushes(coords)
	if len(got) != 3 {
		t.Fatalf("full clear moved %d panes, want 3", len(got))
	}
	for _, p := range got {
		if p.Provider != "" || p.Model != "" {
			t.Errorf("clear push carries a selection: %+v", p)
		}
		if !isInheritedScope(p.Scope) {
			t.Errorf("clear push %+v addresses the pane's own slot", p)
		}
	}
}

// TestPlanInheritedPushes_ForgetsDeadPanes: a pane that disappears must not
// leave a fingerprint behind, or a recycled id would skip its push.
func TestPlanInheritedPushes_ForgetsDeadPanes(t *testing.T) {
	w := newScopeWorkspace(t)
	w.setModelBinding(scopeLane, "l1", bindingFor("lane-model"))
	live := []paneCoords{{TabID: "t1", LaneID: "l1", GroupID: "g1", PaneID: "p1"}}

	if got := w.planInheritedPushes(live); len(got) != 1 {
		t.Fatalf("first plan = %+v", got)
	}
	// p1 closes.
	if got := w.planInheritedPushes(nil); len(got) != 0 {
		t.Fatalf("empty tree plan = %+v", got)
	}
	if _, stale := w.paneInheritedModel["p1"]; stale {
		t.Fatal("closed pane left a fingerprint behind")
	}
	// A new pane reusing the id still gets its push.
	if got := w.planInheritedPushes(live); len(got) != 1 {
		t.Fatalf("recycled pane id was skipped: %+v", got)
	}
}
