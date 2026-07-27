package forge

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

func TestDiffAPIs(t *testing.T) {
	oldAPI := &ir.API{Operations: []ir.Operation{
		{ID: "getUser", Method: "GET", Path: "/users/{id}", Params: []ir.Param{{Name: "id", In: "path", Required: true}}},
		{ID: "delUser", Method: "DELETE", Path: "/users/{id}", Mutating: true},
		{ID: "stay", Method: "GET", Path: "/x"},
	}}
	newAPI := &ir.API{Operations: []ir.Operation{
		// getUser gains a query param → changed
		{ID: "getUser", Method: "GET", Path: "/users/{id}", Params: []ir.Param{{Name: "id", In: "path", Required: true}, {Name: "expand", In: "query"}}},
		{ID: "createUser", Method: "POST", Path: "/users", Mutating: true}, // added
		{ID: "stay", Method: "GET", Path: "/x"},                            // unchanged
	}}

	d := DiffAPIs(oldAPI, newAPI)
	if len(d.Added) != 1 || d.Added[0] != "createUser" {
		t.Fatalf("added = %v, want [createUser]", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0] != "delUser" {
		t.Fatalf("removed = %v, want [delUser]", d.Removed)
	}
	if len(d.Changed) != 1 || d.Changed[0] != "getUser" {
		t.Fatalf("changed = %v, want [getUser]", d.Changed)
	}
	if d.Empty() {
		t.Fatalf("diff should not be empty")
	}
	if !DiffAPIs(newAPI, newAPI).Empty() {
		t.Fatalf("identical specs should produce an empty diff")
	}
}
