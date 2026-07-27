package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// TestRunOpExecutesForgeOp verifies RunOp runs a forge-registered op with real
// credentials (it reaches the upstream server) and returns its output.
func TestRunOpExecutesForgeOp(t *testing.T) {
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"p1","name":"Rex"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	rel, err := StoreSpec(dir, "pet", specFor(srv.URL), "json")
	if err != nil {
		t.Fatalf("StoreSpec: %v", err)
	}
	if err := SaveStore(dir, []Integration{{Name: "pet", Source: SourceOpenAPI, SpecFile: rel}}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}

	global := sharedtools.NewToolRegistry()
	global.Register("bash", builtinStub{name: "bash"}) // a built-in that must NOT be runnable via RunOp
	mgr := NewManager(global, dir, nil)
	if _, _, err := mgr.EnableByName(context.Background(), "pet", mgr.GlobalTarget()); err != nil {
		t.Fatalf("enable: %v", err)
	}

	out, err := mgr.RunOp(context.Background(), "pet_getPet", json.RawMessage(`{"id":"p1"}`))
	if err != nil {
		t.Fatalf("RunOp: %v", err)
	}
	if hitPath != "/pets/p1" {
		t.Fatalf("upstream path = %q, want /pets/p1", hitPath)
	}
	if !strings.Contains(out.Content, "Rex") {
		t.Fatalf("unexpected output: %+v", out)
	}
}

// TestRunOpRejectsNonForge verifies the forge-origin guarantee: a built-in name
// (registered in the same registry) and an unknown name both fail the lookup —
// RunOp can never run a non-forge executor.
func TestRunOpRejectsNonForge(t *testing.T) {
	dir := t.TempDir()
	rel, _ := StoreSpec(dir, "pet", specFor("http://example.test"), "json")
	_ = SaveStore(dir, []Integration{{Name: "pet", Source: SourceOpenAPI, SpecFile: rel}})

	global := sharedtools.NewToolRegistry()
	global.Register("bash", builtinStub{name: "bash"})
	mgr := NewManager(global, dir, nil)
	if _, _, err := mgr.EnableByName(context.Background(), "pet", mgr.GlobalTarget()); err != nil {
		t.Fatalf("enable: %v", err)
	}

	if _, err := mgr.RunOp(context.Background(), "bash", json.RawMessage(`{}`)); err == nil {
		t.Fatalf("RunOp(bash) should fail the forge-origin check")
	}
	if _, err := mgr.RunOp(context.Background(), "does_not_exist", nil); err == nil {
		t.Fatalf("RunOp(unknown) should fail")
	}
}

// TestAllowSharedOp covers the default-deny-mutation gate.
func TestAllowSharedOp(t *testing.T) {
	cases := []struct {
		name     string
		op       string
		mutating bool
		allow    []string
		block    []string
		want     bool
	}{
		{"read default allowed", "pet_getPet", false, nil, nil, true},
		{"mutation default denied", "pet_createPet", true, nil, nil, false},
		{"mutation opted in by glob", "pet_createPet", true, []string{"pet_*"}, nil, true},
		{"mutation not matched by allow", "pet_deletePet", true, []string{"pet_create*"}, nil, false},
		{"read denied when allow set and no match", "pet_getPet", false, []string{"other_*"}, nil, false},
		{"read allowed when allow matches", "pet_getPet", false, []string{"pet_*"}, nil, true},
		{"block beats allow", "pet_createPet", true, []string{"pet_*"}, []string{"pet_create*"}, false},
		{"block denies a read", "pet_getPet", false, nil, []string{"pet_get*"}, false},
		{"malformed glob never matches", "pet_getPet", false, []string{"[bad"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AllowSharedOp(c.op, c.mutating, c.allow, c.block); got != c.want {
				t.Fatalf("AllowSharedOp(%q, mut=%v, allow=%v, block=%v) = %v, want %v",
					c.op, c.mutating, c.allow, c.block, got, c.want)
			}
		})
	}
}
