// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSetUIBlockPreservesUnrelatedKeys is the delta-write contract (design 008
// TO2): onboarding must never clobber a config the user has edited by hand.
func TestSetUIBlockPreservesUnrelatedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rysh.config.yaml")
	original := `# my config — this comment must survive
provider:
  name: anthropic
  api_key: ${ANTHROPIC_API_KEY}
ui:
  shell: /bin/zsh
  shell_history_size: 5000
upstream:
  enabled: true
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SetUIBlock(path, "/bin/fish", 2, 3); err != nil {
		t.Fatalf("SetUIBlock: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	// Unrelated keys survive.
	for _, want := range []string{
		"shell_history_size: 5000", // a ui: key we did not touch
		"name: anthropic",          // another block entirely
		"enabled: true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lost unrelated key %q:\n%s", want, got)
		}
	}
	// Our keys are updated in place.
	if !strings.Contains(got, "shell: /bin/fish") {
		t.Errorf("shell not updated:\n%s", got)
	}

	// And the result still parses into the real config shape, with ints as
	// ints — a !!str "2" here would break Load with a type error.
	var probe struct {
		UI struct {
			Shell            string `yaml:"shell"`
			InitialTabs      int    `yaml:"initial_tabs"`
			InitialPanes     int    `yaml:"initial_panes"`
			ShellHistorySize int    `yaml:"shell_history_size"`
		} `yaml:"ui"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, got)
	}
	if probe.UI.Shell != "/bin/fish" || probe.UI.InitialTabs != 2 || probe.UI.InitialPanes != 3 {
		t.Errorf("ui = %+v", probe.UI)
	}
	if probe.UI.ShellHistorySize != 5000 {
		t.Errorf("unrelated ui key lost: %+v", probe.UI)
	}
}

// TestSetUIBlockCreatesBlockAndIsIdempotent covers the fresh-config path and
// the wizard's re-run contract: a second identical write changes nothing.
func TestSetUIBlockCreatesBlockAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rysh.config.yaml")

	if err := SetUIBlock(path, "/bin/bash", 1, 1); err != nil {
		t.Fatalf("SetUIBlock on missing file: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "shell: /bin/bash") {
		t.Errorf("block not created:\n%s", first)
	}

	if err := SetUIBlock(path, "/bin/bash", 1, 1); err != nil {
		t.Fatalf("second SetUIBlock: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("re-run was not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestSetUIBlockZeroArgsAreNoOps: empty/zero values leave keys untouched, so a
// partial answer never resets a setting the user did not mention.
func TestSetUIBlockZeroArgsAreNoOps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("ui:\n  shell: /bin/zsh\n  initial_tabs: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only the shell is supplied; the tab count must survive.
	if err := SetUIBlock(path, "/bin/fish", 0, 0); err != nil {
		t.Fatalf("SetUIBlock: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "initial_tabs: 4") {
		t.Errorf("zero arg clobbered an existing key:\n%s", raw)
	}
	if !strings.Contains(string(raw), "shell: /bin/fish") {
		t.Errorf("shell not written:\n%s", raw)
	}

	// All-zero is a no-op, not an error and not a write.
	before, _ := os.ReadFile(path)
	if err := SetUIBlock(path, "", 0, 0); err != nil {
		t.Fatalf("all-zero SetUIBlock should be a no-op: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("all-zero SetUIBlock modified the file")
	}
}

func TestSetUIBlockRejectsNonMapping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("ui: \"not a mapping\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetUIBlock(path, "/bin/sh", 0, 0); err == nil {
		t.Error("expected an error when `ui` is not a mapping")
	}
}

// TestShellPrecedence pins the fix for a bug found while implementing design
// 008 TO2: $SHELL was re-applied AFTER the config file, so `ui.shell` was dead
// configuration on any machine with $SHELL set (i.e. every machine). It is a
// documented key that `rysh onboard` writes, so it has to win over the ambient
// default; only an explicit RYSH_SHELL outranks it.
func TestShellPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("ui:\n  shell: /bin/fish\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("RYSH_SHELL", "")
	if got := LoadFrom(path).DefaultShell; got != "/bin/fish" {
		t.Errorf("config ui.shell should beat ambient $SHELL: got %q, want /bin/fish", got)
	}

	// An explicit RYSH_SHELL still wins.
	t.Setenv("RYSH_SHELL", "/bin/dash")
	if got := LoadFrom(path).DefaultShell; got != "/bin/dash" {
		t.Errorf("RYSH_SHELL should override the config: got %q, want /bin/dash", got)
	}

	// With no config key, $SHELL remains the baseline.
	t.Setenv("RYSH_SHELL", "")
	empty := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(empty, []byte("provider:\n  name: anthropic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadFrom(empty).DefaultShell; got != "/bin/zsh" {
		t.Errorf("ambient $SHELL should still be the baseline: got %q, want /bin/zsh", got)
	}
}
