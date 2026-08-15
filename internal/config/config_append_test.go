// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendWorkspace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rysh.config.yaml")
	initial := "# my rysh config\n" +
		"upstream:\n" +
		"  enabled: true\n" +
		"  url: \"https://rysh.ai\"\n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := AppendWorkspace(path, "halil-macbook", "rysh_KEY123"); err != nil {
		t.Fatalf("AppendWorkspace 1: %v", err)
	}
	if err := AppendWorkspace(path, "halil-macbook-2", "rysh_KEY456"); err != nil {
		t.Fatalf("AppendWorkspace 2: %v", err)
	}

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# my rysh config") {
		t.Errorf("leading comment not preserved:\n%s", raw)
	}
	// The portable "~/" form is what gets persisted.
	if !strings.Contains(string(raw), "working_directory: ~/") {
		t.Errorf("working_directory not persisted as ~/:\n%s", raw)
	}

	cfg := LoadFrom(path)
	if len(cfg.Workspaces) != 2 {
		t.Fatalf("expected 2 workspaces, got %d:\n%s", len(cfg.Workspaces), raw)
	}
	w0 := cfg.Workspaces[0]
	// LoadFrom expands ~/ to the home dir, so WorkingDirectory is non-empty.
	if w0.Name != "halil-macbook" || w0.WorkingDirectory == "" {
		t.Errorf("ws[0] = %+v (raw:\n%s)", w0, raw)
	}
	if w0.Upstream == nil || !w0.Upstream.Enabled || w0.Upstream.APIKey != "rysh_KEY123" {
		t.Errorf("ws[0].Upstream = %+v", w0.Upstream)
	}
	w1 := cfg.Workspaces[1]
	if w1.Name != "halil-macbook-2" || w1.Upstream == nil || w1.Upstream.APIKey != "rysh_KEY456" {
		t.Errorf("ws[1] = %+v", w1)
	}
}

// AppendWorkspace must also create the workspace: key when the config has none.
func TestAppendWorkspace_NoExistingList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("rysh:\n  initial_panes: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendWorkspace(path, "solo", "rysh_SOLO"); err != nil {
		t.Fatalf("AppendWorkspace: %v", err)
	}
	cfg := LoadFrom(path)
	if len(cfg.Workspaces) != 1 || cfg.Workspaces[0].Name != "solo" {
		raw, _ := os.ReadFile(path)
		t.Fatalf("expected 1 workspace 'solo', got %+v (raw:\n%s)", cfg.Workspaces, raw)
	}
}
