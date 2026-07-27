package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLayeredPromptStore_EmbeddedFallback: with no override, reads come
// from the embedded FS.
func TestLayeredPromptStore_EmbeddedFallback(t *testing.T) {
	store := &layeredPromptStore{embedded: PromptsFS()}
	got := store.Load("system_default.md")
	if got == "" || !strings.Contains(got, "expert software engineer") {
		t.Errorf("expected embedded default prompt, got %q", got)
	}
}

// TestLayeredPromptStore_OverrideWins: a file in the override dir takes
// precedence over the embedded baseline.
func TestLayeredPromptStore_OverrideWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "system_default.md"), []byte("OVERRIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &layeredPromptStore{overrideDir: dir, embedded: PromptsFS()}
	if got := store.Load("system_default.md"); got != "OVERRIDE" {
		t.Errorf("expected OVERRIDE, got %q", got)
	}
}

// TestLayeredPromptStore_OverrideDirEmpty: an empty / nonexistent
// override dir falls through to the embedded FS.
func TestLayeredPromptStore_OverrideDirEmpty(t *testing.T) {
	store := &layeredPromptStore{overrideDir: "/path/does/not/exist", embedded: PromptsFS()}
	got := store.Load("system_default.md")
	if got == "" {
		t.Errorf("expected embedded fallback when override dir missing")
	}
}

// TestLayeredPromptStore_MissingFile yields empty string when neither
// tier has the file (callers substitute their own fallback consts).
func TestLayeredPromptStore_MissingFile(t *testing.T) {
	store := &layeredPromptStore{embedded: PromptsFS()}
	if got := store.Load("does-not-exist.md"); got != "" {
		t.Errorf("expected empty for missing file, got %q", got)
	}
}

// TestLayeredPromptStore_Inventory lists distinct names from both tiers.
func TestLayeredPromptStore_Inventory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "custom_extra.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &layeredPromptStore{overrideDir: dir, embedded: PromptsFS()}
	inv := store.Inventory()
	seen := map[string]bool{}
	for _, n := range inv {
		seen[n] = true
	}
	if !seen["system_default.md"] {
		t.Errorf("expected embedded system_default.md in inventory")
	}
	if !seen["custom_extra.md"] {
		t.Errorf("expected override custom_extra.md in inventory")
	}
}

// TestInitPromptStore_NoEnv: when RYSH_PROMPTS_DIR is unset and
// XDG_CONFIG_HOME is unset, we still get a working store (the override
// may be "" because the dir doesn't exist).
func TestInitPromptStore_NoEnv(t *testing.T) {
	t.Setenv("RYSH_PROMPTS_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/path/does/not/exist")
	store := initPromptStore()
	if got := store.Load("system_default.md"); got == "" {
		t.Errorf("expected embedded prompt to load when override absent")
	}
}

// TestInitPromptStore_WithOverride: an explicit RYSH_PROMPTS_DIR
// pointing at an existing dir is honoured.
func TestInitPromptStore_WithOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "system_default.md"), []byte("EX"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RYSH_PROMPTS_DIR", dir)
	store := initPromptStore()
	if got := store.Load("system_default.md"); got != "EX" {
		t.Errorf("expected EX from override, got %q", got)
	}
}
