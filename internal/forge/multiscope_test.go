package forge

import (
	"context"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// petManager stores the pet spec and returns a manager + two sibling lane scopes.
func petManager(t *testing.T) (*Manager, *sharedtools.ToolRegistry, *sharedtools.ToolRegistry, *sharedtools.ToolRegistry) {
	t.Helper()
	dir := t.TempDir()
	rel, err := StoreSpec(dir, "pet", specFor("http://example.test"), "json")
	if err != nil {
		t.Fatalf("StoreSpec: %v", err)
	}
	if err := SaveStore(dir, []Integration{{Name: "pet", Source: SourceOpenAPI, SpecFile: rel}}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
	global := sharedtools.NewToolRegistry()
	mgr := NewManager(global, dir, nil)
	return mgr, global, sharedtools.NewChildRegistry(global), sharedtools.NewChildRegistry(global)
}

// TestMultiScopeEnable verifies the same integration can be enabled at two scopes
// at once, that each scope sees the tools, List aggregates, and per-scope
// teardown removes one without affecting the other.
func TestMultiScopeEnable(t *testing.T) {
	mgr, global, laneA, laneB := petManager(t)
	ctx := context.Background()

	if _, _, err := mgr.EnableByName(ctx, "pet", ScopeTarget{Key: "lane:L1", Registry: laneA}); err != nil {
		t.Fatalf("enable L1: %v", err)
	}
	if _, _, err := mgr.EnableByName(ctx, "pet", ScopeTarget{Key: "lane:L2", Registry: laneB}); err != nil {
		t.Fatalf("enable L2: %v", err)
	}

	// Both scopes have the tools; global (the shared root) does not.
	if _, ok := laneA.Get("pet_getPet"); !ok {
		t.Fatalf("pet_getPet missing from lane A")
	}
	if _, ok := laneB.Get("pet_getPet"); !ok {
		t.Fatalf("pet_getPet missing from lane B")
	}
	if _, ok := global.Get("pet_getPet"); ok {
		t.Fatalf("scoped tool leaked into the global registry")
	}

	// List aggregates: scope shows both, tool count is summed across scopes.
	st := findStatus(t, mgr, "pet")
	if st.Scope != "lane:L1,lane:L2" {
		t.Fatalf("aggregated scope = %q, want lane:L1,lane:L2", st.Scope)
	}
	if st.ToolCount != 4 { // 2 ops × 2 scopes
		t.Fatalf("aggregated tool count = %d, want 4", st.ToolCount)
	}

	// Tools(name) unions across scopes (deduped) — 2 distinct ops.
	names, ok := mgr.Tools("pet")
	if !ok || len(names) != 2 {
		t.Fatalf("Tools union = %v (ok=%v), want 2 distinct", names, ok)
	}

	// Tear down lane L1 only: its tools go, lane L2 keeps them.
	mgr.UnregisterScope("lane:L1")
	if _, ok := laneA.Get("pet_getPet"); ok {
		t.Fatalf("lane A tools should be gone after UnregisterScope(L1)")
	}
	if _, ok := laneB.Get("pet_getPet"); !ok {
		t.Fatalf("lane B tools should remain after UnregisterScope(L1)")
	}
	st = findStatus(t, mgr, "pet")
	if st.Scope != "lane:L2" {
		t.Fatalf("after L1 teardown, scope = %q, want lane:L2", st.Scope)
	}

	// Disable removes the integration from EVERY remaining scope.
	if err := mgr.Disable("pet"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, ok := laneB.Get("pet_getPet"); ok {
		t.Fatalf("lane B tools should be gone after Disable")
	}
	if _, ok := mgr.Tools("pet"); ok {
		t.Fatalf("Tools should report pet not enabled after Disable")
	}
}

// TestMultiScopeSharedOpsDedup verifies SharedOps returns each op once even when
// the integration is enabled at multiple scopes that all match the query.
func TestMultiScopeSharedOpsDedup(t *testing.T) {
	mgr, _, _, tabReg := petManager(t)
	ctx := context.Background()

	if _, _, err := mgr.EnableByName(ctx, "pet", mgr.GlobalTarget()); err != nil {
		t.Fatalf("enable global: %v", err)
	}
	if _, _, err := mgr.EnableByName(ctx, "pet", ScopeTarget{Key: "tab:T1", Registry: tabReg}); err != nil {
		t.Fatalf("enable tab: %v", err)
	}

	ops := mgr.SharedOps(map[string]bool{"global": true, "tab:T1": true})
	seen := map[string]int{}
	for _, o := range ops {
		seen[o.Name]++
	}
	for name, c := range seen {
		if c != 1 {
			t.Fatalf("op %q appeared %d times (want 1) — SharedOps did not dedup across scopes", name, c)
		}
	}
	if seen["pet_getPet"] == 0 || seen["pet_createPet"] == 0 {
		t.Fatalf("expected both pet ops, got %v", seen)
	}
}

func findStatus(t *testing.T, mgr *Manager, name string) Status {
	t.Helper()
	for _, s := range mgr.List() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("integration %q not in list", name)
	return Status{}
}
