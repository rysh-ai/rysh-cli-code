package forge

import (
	"context"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// TestAPIOps verifies APIOps returns an integration's ops by name (deduped),
// is empty before enable / for unknown names, and works regardless of scope.
func TestAPIOps(t *testing.T) {
	mgr, _, lane, _ := petManager(t)
	ctx := context.Background()

	if ops := mgr.APIOps("pet"); len(ops) != 0 {
		t.Fatalf("expected no ops before enable, got %d", len(ops))
	}
	if _, _, err := mgr.EnableByName(ctx, "pet", ScopeTarget{Key: "lane:L1", Registry: lane}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	ops := mgr.APIOps("pet")
	names := map[string]bool{}
	mut := map[string]bool{}
	for _, o := range ops {
		names[o.Name] = true
		mut[o.Name] = o.Mutating
	}
	if !names["pet_getPet"] || !names["pet_createPet"] {
		t.Fatalf("APIOps missing ops: %v", names)
	}
	if !mut["pet_createPet"] || mut["pet_getPet"] {
		t.Fatalf("mutating flags wrong: %v", mut)
	}
	if ops := mgr.APIOps("does-not-exist"); len(ops) != 0 {
		t.Fatalf("unknown integration should yield no ops, got %d", len(ops))
	}

	// Multi-scope dedup: enabling at a second scope must not duplicate ops.
	other := sharedtools.NewChildRegistry(mgr.GlobalTarget().Registry)
	if _, _, err := mgr.EnableByName(ctx, "pet", ScopeTarget{Key: "tab:T1", Registry: other}); err != nil {
		t.Fatalf("enable 2nd scope: %v", err)
	}
	if got := len(mgr.APIOps("pet")); got != 2 {
		t.Fatalf("APIOps should dedup across scopes; got %d, want 2", got)
	}
}
