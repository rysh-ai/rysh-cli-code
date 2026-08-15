// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"testing"
)

// ParseRunSkillFile is the seam `rysh run <skill.md>` (design 009) uses to
// reuse this package's skill parsing: frontmatter, ${VAR} expansion from the
// environment, and isolation validation must all apply.
func TestParseRunSkillFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deployer.md")
	t.Setenv("RUN_SKILL_TEST_TARGET", "staging")
	if err := os.WriteFile(path, []byte(`---
name: deployer
isolation: worktree
---
Deploy to ${RUN_SKILL_TEST_TARGET} and report.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	sk, err := ParseRunSkillFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "deployer" || sk.Isolation != "worktree" {
		t.Fatalf("frontmatter not parsed: %+v", sk)
	}
	if sk.Prompt != "Deploy to staging and report." {
		t.Fatalf("body must be ${VAR}-expanded, got %q", sk.Prompt)
	}
}

func TestParseRunSkillFile_InvalidIsolationRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "typo.md")
	if err := os.WriteFile(path, []byte("---\nisolation: sandbox\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRunSkillFile(path); err == nil {
		t.Fatal("invalid isolation must be rejected, not silently ignored")
	}
}
