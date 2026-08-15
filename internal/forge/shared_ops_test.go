// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"encoding/json"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// builtinStub stands in for a non-forge (built-in) tool living in the global
// registry — it must never appear in SharedOps.
type builtinStub struct{ name string }

func (b builtinStub) Execute(context.Context, json.RawMessage) (*sharedtools.ToolOutput, error) {
	return &sharedtools.ToolOutput{}, nil
}
func (b builtinStub) Spec() sharedtools.ToolSpec            { return sharedtools.ToolSpec{Name: b.name} }
func (b builtinStub) RequiresApproval(json.RawMessage) bool { return false }

// TestSharedOpsForgeOriginFilter verifies SharedOps returns only forge-registered
// operations at the given scope (the forge-origin guarantee), with mutation flags
// from the op method, and excludes built-ins and other scopes.
func TestSharedOpsForgeOriginFilter(t *testing.T) {
	dir := t.TempDir()
	rel, err := StoreSpec(dir, "pet", specFor("http://example.test"), "json")
	if err != nil {
		t.Fatalf("StoreSpec: %v", err)
	}
	if err := SaveStore(dir, []Integration{{Name: "pet", Source: SourceOpenAPI, SpecFile: rel}}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}

	global := sharedtools.NewToolRegistry()
	global.Register("bash", builtinStub{name: "bash"}) // a built-in in the global registry
	mgr := NewManager(global, dir, nil)
	lane := sharedtools.NewChildRegistry(global)
	if _, _, err := mgr.EnableByName(context.Background(), "pet", ScopeTarget{Key: "lane:L1", Registry: lane}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	ops := mgr.SharedOps(map[string]bool{"lane:L1": true})
	names := map[string]bool{}
	mutating := map[string]bool{}
	for _, o := range ops {
		names[o.Name] = true
		mutating[o.Name] = o.Mutating
	}
	if !names["pet_getPet"] {
		t.Fatalf("forge op pet_getPet missing from SharedOps: %v", names)
	}
	if names["bash"] {
		t.Fatalf("built-in 'bash' leaked into SharedOps (forge-origin filter failed)")
	}
	if !mutating["pet_createPet"] {
		t.Fatalf("pet_createPet (POST) should be marked Mutating: %+v", ops)
	}
	if mutating["pet_getPet"] {
		t.Fatalf("pet_getPet (GET) should not be Mutating")
	}

	// A non-matching scope returns nothing.
	if got := mgr.SharedOps(map[string]bool{"lane:OTHER": true}); len(got) != 0 {
		t.Fatalf("expected no ops for a different scope, got %d", len(got))
	}
}
