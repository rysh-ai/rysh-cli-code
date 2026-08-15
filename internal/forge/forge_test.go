// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

func specFor(baseURL string) []byte {
	return []byte(fmt.Sprintf(`{
  "openapi": "3.0.0",
  "info": {"title": "Pet", "version": "1.0.0"},
  "servers": [{"url": %q}],
  "paths": {
    "/pets/{id}": {"get": {"operationId": "getPet",
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "responses": {"200": {"description": "ok"}}}},
    "/pets": {"post": {"operationId": "createPet", "responses": {"201": {"description": "created"}}}}
  }
}`, baseURL))
}

func TestManagerEnableEndToEnd(t *testing.T) {
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
	def := Integration{Name: "pet", Source: SourceOpenAPI, SpecFile: rel, Enabled: true}
	if err := SaveStore(dir, []Integration{def}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}

	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, dir, nil)

	n, mode, err := mgr.EnableByName(context.Background(), "pet", mgr.GlobalTarget())
	if err != nil {
		t.Fatalf("EnableByName: %v", err)
	}
	if n != 2 || mode != "static" {
		t.Fatalf("enable result: n=%d mode=%q", n, mode)
	}

	// The registered tool actually calls the upstream server.
	getPet, ok := reg.Get("pet_getPet")
	if !ok {
		t.Fatalf("pet_getPet not registered; names=%v", reg.Names())
	}
	out, err := getPet.Execute(context.Background(), json.RawMessage(`{"id":"p1"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if hitPath != "/pets/p1" {
		t.Fatalf("upstream path = %q, want /pets/p1", hitPath)
	}
	if !strings.Contains(out.Content, "Rex") {
		t.Fatalf("unexpected output: %+v", out)
	}

	// List reflects the enabled integration.
	items := mgr.List()
	if len(items) != 1 || !items[0].Enabled || items[0].ToolCount != 2 {
		t.Fatalf("list: %+v", items)
	}

	// Disable unregisters the tools.
	if err := mgr.Disable("pet"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, ok := reg.Get("pet_getPet"); ok {
		t.Fatalf("tool still registered after disable")
	}
}

func TestManagerRejectsGraphQLLive(t *testing.T) {
	dir := t.TempDir()
	rel, _ := StoreSpec(dir, "gql", []byte(`{"data":{"__schema":{}}}`), "json")
	def := Integration{Name: "gql", Source: SourceGraphQL, SpecFile: rel, Enabled: true}
	_ = SaveStore(dir, []Integration{def})
	mgr := NewManager(sharedtools.NewToolRegistry(), dir, nil)
	if _, _, err := mgr.EnableByName(context.Background(), "gql", mgr.GlobalTarget()); err == nil {
		t.Fatalf("expected graphql live enable to be rejected")
	}
}

// TestManagerEnableScoped verifies an integration enables into a caller-supplied
// scope target (a child registry), so its tools are visible there but not in a
// sibling scope, and Disable retracts from the right target.
func TestManagerEnableScoped(t *testing.T) {
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
	lane := sharedtools.NewChildRegistry(global)  // a "lane:L1" scope registry
	other := sharedtools.NewChildRegistry(global) // a sibling lane

	if _, _, err := mgr.EnableByName(context.Background(), "pet", ScopeTarget{Key: "lane:L1", Registry: lane}); err != nil {
		t.Fatalf("scoped EnableByName: %v", err)
	}
	// Visible in the lane it was enabled into (Clone flattens the chain)...
	if _, ok := lane.Clone().Get("pet_getPet"); !ok {
		t.Fatalf("tool not visible in its own lane scope")
	}
	// ...but not in a sibling lane, nor directly at global.
	if _, ok := other.Clone().Get("pet_getPet"); ok {
		t.Fatalf("scoped tool leaked into a sibling scope")
	}
	if _, ok := global.Get("pet_getPet"); ok {
		t.Fatalf("scoped tool leaked into the global registry")
	}
	// List reports the scope it was enabled at.
	items := mgr.List()
	if len(items) != 1 || items[0].Scope != "lane:L1" {
		t.Fatalf("list scope: %+v", items)
	}
	// Disable retracts from the lane target.
	if err := mgr.Disable("pet"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, ok := lane.Clone().Get("pet_getPet"); ok {
		t.Fatalf("tool still present after scoped disable")
	}
}

// TestManagerReplayScope verifies per-scope persistence: a scoped enablement is
// NOT re-applied at global on Bootstrap, but IS replayed into the matching scope
// registry on ReplayScope (and only the matching scope).
func TestManagerReplayScope(t *testing.T) {
	dir := t.TempDir()
	rel, err := StoreSpec(dir, "pet", specFor("http://example.test"), "json")
	if err != nil {
		t.Fatalf("StoreSpec: %v", err)
	}
	if err := SaveStore(dir, []Integration{{Name: "pet", Source: SourceOpenAPI, SpecFile: rel, Enabled: true, Scope: "lane:L1"}}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}

	global := sharedtools.NewToolRegistry()
	mgr := NewManager(global, dir, nil)

	// Bootstrap must NOT register a scoped enablement at global.
	mgr.Bootstrap(context.Background())
	if _, ok := global.Get("pet_getPet"); ok {
		t.Fatalf("scoped integration must not bootstrap at global")
	}

	// ReplayScope("lane:L1") re-applies it into the lane registry.
	lane := sharedtools.NewChildRegistry(global)
	mgr.ReplayScope(context.Background(), "lane:L1", ScopeTarget{Key: "lane:L1", Registry: lane})
	if _, ok := lane.Clone().Get("pet_getPet"); !ok {
		t.Fatalf("ReplayScope did not register the tool into the lane")
	}

	// A non-matching scope key replays nothing.
	other := sharedtools.NewChildRegistry(global)
	mgr.ReplayScope(context.Background(), "lane:OTHER", ScopeTarget{Key: "lane:OTHER", Registry: other})
	if _, ok := other.Clone().Get("pet_getPet"); ok {
		t.Fatalf("ReplayScope matched the wrong scope")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if defs, err := LoadStore(dir); err != nil || len(defs) != 0 {
		t.Fatalf("empty load: %v %v", defs, err)
	}
	in := []Integration{{Name: "a", Source: "openapi", SpecFile: "x.json"}}
	if err := SaveStore(dir, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadStore(dir)
	if err != nil || len(out) != 1 || out[0].Name != "a" {
		t.Fatalf("reload: %v %v", out, err)
	}
}
