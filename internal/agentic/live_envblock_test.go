package agentic

import (
	"strings"
	"testing"
)

// TestApplyPromptsSeparatesEnvBlock verifies that ApplyPrompts produces two
// prompts: SystemPrompt with the (static) env block for headless agents, and
// SystemPromptNoEnv without it for panes (which inject a live env block per
// turn). Both retain the base persona and todo guidance.
func TestApplyPromptsSeparatesEnvBlock(t *testing.T) {
	staticDir := t.TempDir()
	p := &Prompts{
		Default:          "BASE PERSONA",
		EnvBlockTemplate: "## Environment\nWorking directory: `{{cwd}}`",
		TodoGuidance:     "TODO GUIDANCE",
	}
	s := &Setup{}
	s.ApplyPrompts(p, staticDir)

	if !strings.Contains(s.SystemPrompt, "Working directory: `"+staticDir+"`") {
		t.Errorf("SystemPrompt should contain the static env block:\n%s", s.SystemPrompt)
	}
	if strings.Contains(s.SystemPromptNoEnv, "Working directory") {
		t.Errorf("SystemPromptNoEnv must NOT contain an env block:\n%s", s.SystemPromptNoEnv)
	}
	for _, want := range []string{"BASE PERSONA", "TODO GUIDANCE"} {
		if !strings.Contains(s.SystemPrompt, want) {
			t.Errorf("SystemPrompt missing %q", want)
		}
		if !strings.Contains(s.SystemPromptNoEnv, want) {
			t.Errorf("SystemPromptNoEnv missing %q", want)
		}
	}
}

// TestBuildLiveEnvBlock verifies the per-turn env block substitutes the live
// working directory passed in (a pane's shell cwd), not the Setup-time dir.
func TestBuildLiveEnvBlock(t *testing.T) {
	staticDir := t.TempDir()
	liveDir := t.TempDir()
	p := &Prompts{
		Default:          "BASE",
		EnvBlockTemplate: "Working directory: `{{cwd}}`",
	}
	s := &Setup{}
	s.ApplyPrompts(p, staticDir)

	env := s.BuildLiveEnvBlock(liveDir)
	if !strings.Contains(env, "Working directory: `"+liveDir+"`") {
		t.Errorf("BuildLiveEnvBlock(%q) = %q, want it to contain the live dir", liveDir, env)
	}
	if strings.Contains(env, staticDir) {
		t.Errorf("BuildLiveEnvBlock leaked the static dir: %q", env)
	}
}

// TestBuildLiveEnvBlockNoPrompts is the nil-safety path: a Setup without
// Prompts returns an empty env block (feature disabled).
func TestBuildLiveEnvBlockNoPrompts(t *testing.T) {
	s := &Setup{}
	if got := s.BuildLiveEnvBlock(t.TempDir()); got != "" {
		t.Errorf("expected empty env block with no Prompts, got %q", got)
	}
}
