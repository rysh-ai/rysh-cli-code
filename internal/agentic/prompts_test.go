package agentic

import (
	"os"
	"strings"
	"testing"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"
)

// TestPromptsResolve_FallbacksWhenEmpty verifies that an empty/nil Prompts
// falls back to the in-package constants — tests can construct a Setup
// without touching the embedded FS.
func TestPromptsResolve_FallbacksWhenEmpty(t *testing.T) {
	var p *Prompts
	if got := p.defaultPrompt(); !strings.Contains(got, "expert software engineer") {
		t.Errorf("nil default fallback unexpected: %q", got)
	}
	if got := p.emailGovernance(); !strings.Contains(got, "HUMAN-GOVERNED email mode") {
		t.Errorf("nil email fallback unexpected: %q", got)
	}
	empty := &Prompts{}
	if got := empty.defaultPrompt(); !strings.Contains(got, "expert software engineer") {
		t.Errorf("empty default fallback unexpected: %q", got)
	}
	if got := empty.todoGuidance(); got != "" {
		t.Errorf("expected empty todo guidance with empty Prompts, got %q", got)
	}
}

// TestPromptsResolve_OverridesWin demonstrates that non-empty field values
// override the fallback.
func TestPromptsResolve_OverridesWin(t *testing.T) {
	p := &Prompts{
		Default:         "OVERRIDDEN DEFAULT",
		EmailGovernance: "OVERRIDDEN EMAIL",
		TodoGuidance:    "OVERRIDDEN TODO",
	}
	if p.defaultPrompt() != "OVERRIDDEN DEFAULT" {
		t.Errorf("default override failed: %q", p.defaultPrompt())
	}
	if p.emailGovernance() != "OVERRIDDEN EMAIL" {
		t.Errorf("email override failed: %q", p.emailGovernance())
	}
	if p.todoGuidance() != "OVERRIDDEN TODO" {
		t.Errorf("todo override failed: %q", p.todoGuidance())
	}
}

// TestApplySharedOverrides verifies that non-empty Sub-agent and Compaction
// fields push down to the rysh-shared package vars.
func TestApplySharedOverrides(t *testing.T) {
	// Snapshot the current shared defaults so the test is reversible.
	originalSub := sharedagentic.DefaultSubAgentSystemPrompt
	originalComp := sharedagentic.DefaultCompactionSummarizePrompt
	t.Cleanup(func() {
		sharedagentic.DefaultSubAgentSystemPrompt = originalSub
		sharedagentic.DefaultCompactionSummarizePrompt = originalComp
	})

	p := &Prompts{
		SubAgent:            "TEST SUB-AGENT PROMPT",
		CompactionSummarize: "TEST COMPACTION PROMPT",
	}
	p.ApplySharedOverrides()
	if sharedagentic.DefaultSubAgentSystemPrompt != "TEST SUB-AGENT PROMPT" {
		t.Errorf("sub-agent override didn't apply: %q", sharedagentic.DefaultSubAgentSystemPrompt)
	}
	if sharedagentic.DefaultCompactionSummarizePrompt != "TEST COMPACTION PROMPT" {
		t.Errorf("compaction override didn't apply: %q", sharedagentic.DefaultCompactionSummarizePrompt)
	}
}

// TestApplyPrompts_AssemblesSystemPrompt: round-trip test that exercises
// default + env block + todo guidance composition.
func TestApplyPrompts_AssemblesSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	// Seed a go.mod so the env block has a recognisable project type.
	if err := os.WriteFile(dir+"/go.mod", []byte("module x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Setup{}
	p := &Prompts{
		Default:          "BASE PROMPT",
		EnvBlockTemplate: "ENV: cwd={{cwd}} type={{project_type}}",
		TodoGuidance:     "TODOS",
	}
	s.ApplyPrompts(p, dir)
	if !strings.HasPrefix(s.SystemPrompt, "BASE PROMPT") {
		t.Errorf("missing base prompt in:\n%s", s.SystemPrompt)
	}
	if !strings.Contains(s.SystemPrompt, "ENV: cwd="+dir) {
		t.Errorf("missing env block substitution in:\n%s", s.SystemPrompt)
	}
	if !strings.Contains(s.SystemPrompt, "TODOS") {
		t.Errorf("missing todo guidance in:\n%s", s.SystemPrompt)
	}
}
