// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAgentSkill creates .rysh/agents/<name>/SKILL.md under the current dir.
func writeAgentSkill(t *testing.T, name, body string) {
	t.Helper()
	dir := filepath.Join(".rysh", "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
}

// TestAgentArtefactDiscovery verifies parseSkillDir finds every <name>/SKILL.md
// artefact under .rysh/agents and extracts its name/model/description.
func TestAgentArtefactDiscovery(t *testing.T) {
	t.Chdir(t.TempDir())

	writeAgentSkill(t, "code-reviewer", `---
name: code-reviewer
description: Reviews code for bugs
model: claude-sonnet-4-20250514
---
You review code.`)

	writeAgentSkill(t, "doc-writer", `---
name: doc-writer
description: Writes documentation
---
You write docs.`)

	defs, err := parseSkillDir("", envOnlyExpand)
	if err != nil {
		t.Fatalf("parseSkillDir: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 artefacts, got %d", len(defs))
	}
	byName := map[string]*agentDefinition{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	if byName["code-reviewer"].Model != "claude-sonnet-4-20250514" {
		t.Errorf("code-reviewer model: got %q", byName["code-reviewer"].Model)
	}
	if byName["doc-writer"].Description != "Writes documentation" {
		t.Errorf("doc-writer description: got %q", byName["doc-writer"].Description)
	}
}

// TestRenderAgentArtefacts verifies the marking: loaded artefacts are tagged
// [loaded], others [not loaded], rows are name-sorted, the model column is
// shown, and the summary counts the loaded ones.
func TestRenderAgentArtefacts(t *testing.T) {
	defs := []*agentDefinition{
		{Name: "code-reviewer", Description: "Reviews code", Model: "claude-sonnet-4-20250514"},
		{Name: "doc-writer", Description: "Writes docs"},
	}
	loaded := map[string]bool{"code-reviewer": true}

	var out strings.Builder
	renderAgentArtefacts(&out, ".rysh/agents", defs, loaded)
	s := out.String()

	if !strings.Contains(s, "2 artefact(s)") {
		t.Errorf("expected artefact count header, got:\n%s", s)
	}
	// code-reviewer sorts before doc-writer.
	if strings.Index(s, "code-reviewer") > strings.Index(s, "doc-writer") {
		t.Errorf("artefacts not name-sorted:\n%s", s)
	}
	if !strings.Contains(s, "code-reviewer") || !strings.Contains(s, "[loaded") {
		t.Errorf("loaded artefact not marked [loaded]:\n%s", s)
	}
	if !strings.Contains(s, "[not loaded") {
		t.Errorf("unloaded artefact not marked [not loaded]:\n%s", s)
	}
	if !strings.Contains(s, "claude-sonnet-4-20250514") {
		t.Errorf("expected model in output:\n%s", s)
	}
	// The model-less artefact shows a "-" placeholder.
	if !strings.Contains(s, "model: -") {
		t.Errorf("expected '-' model placeholder for doc-writer:\n%s", s)
	}
	if !strings.Contains(s, "1 of 2 loaded") {
		t.Errorf("expected loaded summary '1 of 2 loaded':\n%s", s)
	}
}
