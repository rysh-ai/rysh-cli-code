// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// skillFrontmatter represents the YAML frontmatter of a skill definition file.
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Model       string `yaml:"model"`
	// Isolation selects where the agent works (design 008): "" (shared
	// checkout, the default) or "worktree" (a per-agent git worktree is
	// provisioned at spawn).
	Isolation string `yaml:"isolation"`
	// AutoApprove is the `auto_approve:` field. Absent (nil) means the
	// default, which is TRUE. An agent is autonomous by construction — it runs
	// with no PTY and no terminal, so an approval dialog has nobody to answer
	// it and the run simply stops. Set `auto_approve: false` to gate every
	// consequential tool call on an explicit approval instead. Policy
	// `always_gate` / `bash.deny` (design 013) still overrides this.
	AutoApprove *bool `yaml:"auto_approve"`
}

// agentDefinition holds the parsed result of a skill file.
type agentDefinition struct {
	Name         string
	Description  string
	Model        string
	Isolation    string
	AutoApprove  *bool // nil = default (true); see skillFrontmatter
	SystemPrompt string
}

// parseSkillFile parses a Claude Code skill-format .md file.
// The format is YAML frontmatter (delimited by ---) followed by the system prompt body.
//
// Example:
//
//	---
//	name: code-reviewer
//	description: Reviews code for quality
//	model: claude-sonnet-5
//	---
//	You are a code reviewer. Review code for...
//
// expand fills ${NAME} placeholders in the system prompt body from the named-value
// stores — secrets (.rysh/secrets) then variables (.rysh/variables) then the
// environment, most-specific scope first. A nil expand falls back to
// environment-only resolution.
func parseSkillFile(path string, expand func(string) string) (*agentDefinition, error) {
	if expand == nil {
		expand = envOnlyExpand
	}
	// Resolve to the skill file itself (an entity dir becomes its SKILL.md).
	path = skillFilePath("agents", path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read skill file: %w", err)
	}

	content := string(data)

	// Match the closing "---" only on its own line so a "---" inside a comment or
	// value (e.g. "# --- section ---") does not truncate the frontmatter.
	fmRaw, body, ok := splitFrontmatter(content)
	if !ok {
		return &agentDefinition{
			Name:         deriveSkillName(path),
			SystemPrompt: strings.TrimSpace(expand(content)),
		}, nil
	}

	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}

	if fm.Name == "" {
		fm.Name = deriveSkillName(path)
	}
	switch fm.Isolation {
	case "", "none", "worktree":
	default:
		// Fail loudly: a typo like `isolation: worktrees` silently running on
		// the shared checkout would defeat the isolation the file asked for.
		return nil, fmt.Errorf("unknown isolation %q (use \"worktree\" or omit)", fm.Isolation)
	}
	if fm.Isolation == "none" {
		fm.Isolation = ""
	}

	return &agentDefinition{
		Name:         fm.Name,
		Description:  fm.Description,
		Model:        fm.Model,
		Isolation:    fm.Isolation,
		AutoApprove:  fm.AutoApprove,
		SystemPrompt: strings.TrimSpace(expand(body)),
	}, nil
}

// parseSkillDir scans a directory whose immediate children are per-agent
// directories, each containing a SKILL.md. Returns the parsed definitions,
// skipping any subdirectory without a readable SKILL.md.
func parseSkillDir(dirPath string, expand func(string) string) ([]*agentDefinition, error) {
	dirPath = resolveRyshPath("agents", dirPath)
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("read skill directory: %w", err)
	}

	var defs []*agentDefinition
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillPath := filepath.Join(dirPath, entry.Name(), "SKILL.md")
		def, err := parseSkillFile(skillPath, expand)
		if err != nil {
			continue
		}
		defs = append(defs, def)
	}
	return defs, nil
}

// RunSkill is the subset of a parsed skill file that the headless CI runner
// consumes (`rysh run <skill.md>`, design 009 §3.1). It exists so `rysh run`
// reuses THIS package's skill-file parsing — frontmatter handling, ${VAR}
// expansion, isolation validation — instead of growing a second parser.
type RunSkill struct {
	Name      string // frontmatter name, or derived from the file path
	Isolation string // "" or "worktree" (validated by parseSkillFile)
	Prompt    string // the file body, ${VAR}-expanded
}

// ParseRunSkillFile parses a skill .md file for a headless run. Environment
// variables are the only ${VAR} source (the run CLI has no session-scoped
// secret/variable stores to consult).
func ParseRunSkillFile(path string) (*RunSkill, error) {
	def, err := parseSkillFile(path, nil)
	if err != nil {
		return nil, err
	}
	return &RunSkill{Name: def.Name, Isolation: def.Isolation, Prompt: def.SystemPrompt}, nil
}
