// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestForgeConfigParsing verifies the forge: block is WORKSPACE-scoped
// (workspace[].forge) and that a top-level forge: is ignored.
func TestForgeConfigParsing(t *testing.T) {
	dir := t.TempDir()
	yaml := `
forge:                      # top-level forge is NOT a thing — must be ignored
  integrations:
    - name: should_be_ignored
      spec: /tmp/x.json

workspace:
  - name: ws1
    forge:
      integrations:
        - name: weather
          spec: /tmp/openapi.json
          base_url: http://localhost:8080
          enable_scope: tab
          share: true
      subscribe:
        - api: weather
          scope: lane
`
	p := filepath.Join(dir, "rysh.config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadFrom(p)

	// The workspace carries the forge config.
	var ws *WorkspaceConfig
	for i := range cfg.Workspaces {
		if cfg.Workspaces[i].Name == "ws1" {
			ws = &cfg.Workspaces[i]
		}
	}
	if ws == nil {
		t.Fatalf("workspace ws1 not parsed: %+v", cfg.Workspaces)
	}
	if len(ws.Forge.Integrations) != 1 {
		t.Fatalf("expected 1 workspace integration, got %d: %+v", len(ws.Forge.Integrations), ws.Forge.Integrations)
	}
	ig := ws.Forge.Integrations[0]
	if ig.Name != "weather" || ig.BaseURL != "http://localhost:8080" || ig.EnableScope != "tab" || !ig.Share || ig.Spec != "/tmp/openapi.json" {
		t.Fatalf("integration parsed wrong: %+v", ig)
	}
	if len(ws.Forge.Subscribe) != 1 || ws.Forge.Subscribe[0].API != "weather" || ws.Forge.Subscribe[0].Scope != "lane" {
		t.Fatalf("subscribe parsed wrong: %+v", ws.Forge.Subscribe)
	}

	// A top-level forge: must NOT populate the global carrier.
	if len(cfg.Forge.Integrations) != 0 {
		t.Fatalf("top-level forge: should be ignored, got %+v", cfg.Forge.Integrations)
	}
}
