package main

// Tests for the setup-rysh composite GitHub Action (action/, design 009 §3.3).
//
// The nontrivial logic lives in bash (action/scripts/*.sh) because that is
// what actually executes on a runner; these Go tests wire it into the repo's
// one test entrypoint (`GOWORK=off go test ./...`, no bats dependency):
//
//   - TestSetupRyshActionYAML parses action.yml with the yaml lib already in
//     go.mod and pins the action's public contract: composite runner, the
//     documented inputs/outputs, and the security invariant that NO input is
//     a credential (keys must arrive via env from secrets, never as inputs
//     that would be logged and retained).
//   - TestSetupRyshScripts executes the hermetic bash test suite
//     (action/test/run_tests.sh): install checksum fail-closed behaviour,
//     version pinning, flag mapping, mode mutual exclusion, fail-on mapping.

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// actionYML mirrors the parts of the composite-action schema we assert on.
type actionYML struct {
	Name   string `yaml:"name"`
	Inputs map[string]struct {
		Description string `yaml:"description"`
		Default     string `yaml:"default"`
	} `yaml:"inputs"`
	Outputs map[string]struct {
		Description string `yaml:"description"`
		Value       string `yaml:"value"`
	} `yaml:"outputs"`
	Runs struct {
		Using string `yaml:"using"`
		Steps []struct {
			Name  string `yaml:"name"`
			ID    string `yaml:"id"`
			Shell string `yaml:"shell"`
			Run   string `yaml:"run"`
			Uses  string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"runs"`
}

func TestSetupRyshActionYAML(t *testing.T) {
	data, err := os.ReadFile("action/action.yml")
	if err != nil {
		t.Fatalf("read action.yml: %v", err)
	}
	var a actionYML
	if err := yaml.Unmarshal(data, &a); err != nil {
		t.Fatalf("action.yml does not parse as YAML: %v", err)
	}

	if a.Runs.Using != "composite" {
		t.Errorf("runs.using = %q, want composite", a.Runs.Using)
	}

	wantInputs := []string{
		"version", "build-from-source", "source-dir",
		"task", "skill-file", "eval", "eval-result",
		"provider", "budget", "timeout", "worktree",
		"fail-on", "result-artifact", "artifact-name",
	}
	for _, name := range wantInputs {
		in, ok := a.Inputs[name]
		if !ok {
			t.Errorf("input %q missing", name)
			continue
		}
		if strings.TrimSpace(in.Description) == "" {
			t.Errorf("input %q has no description", name)
		}
	}
	for name := range a.Inputs {
		// Security invariant (design 009 / README): credentials are never
		// action inputs — inputs are logged in debug runs and echoed by
		// forks; keys come via env from secrets.
		lower := strings.ToLower(name)
		for _, bad := range []string{"key", "token", "secret", "password"} {
			if strings.Contains(lower, bad) {
				t.Errorf("input %q looks like a credential input; keys must be provided via env, never inputs", name)
			}
		}
	}

	for _, name := range []string{"status", "exit-code", "result-path", "tap-path"} {
		out, ok := a.Outputs[name]
		if !ok {
			t.Errorf("output %q missing", name)
			continue
		}
		if !strings.Contains(out.Value, "steps.run.outputs") {
			t.Errorf("output %q value %q is not wired to the run step", name, out.Value)
		}
	}

	// The two script steps must call the files that the bash tests cover.
	var sawInstall, sawRun bool
	for _, s := range a.Runs.Steps {
		if strings.Contains(s.Run, "scripts/install.sh") {
			sawInstall = true
		}
		if strings.Contains(s.Run, "scripts/run.sh") {
			if s.ID != "run" {
				t.Errorf("run step id = %q, want \"run\" (outputs reference steps.run)", s.ID)
			}
			sawRun = true
		}
	}
	if !sawInstall || !sawRun {
		t.Errorf("composite steps must invoke scripts/install.sh and scripts/run.sh (install=%v run=%v)", sawInstall, sawRun)
	}
}

func TestSetupRyshScripts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("action scripts are bash; the action does not support native windows")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command(bash, "action/test/run_tests.sh")
	out, err := cmd.CombinedOutput()
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("action/test/run_tests.sh failed: %v", err)
	}
}
