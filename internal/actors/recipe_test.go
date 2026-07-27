package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// slackRecipe is a humanoid skill file that mixes a ${VAR} credential reference
// with a literal one — the two cases `##humanoid show` must treat differently.
const slackRecipe = `---
name: slack-bot
description: Slack test humanoid
model: claude-sonnet-5
contacts:
  slack:
    bot_token: "${SLACK_BOT_TOKEN}"
    app_token: "xapp-1-LITERAL-TOKEN"
    channels:
      - "#rysh-test"
---
You are a Slack assistant.
Answer briefly.`

// TestRedactRecipeSecretsKeepsPlaceholdersMasksLiterals: the recipe view prints
// a file that may be echoed into a shared pane, so a literal credential is
// masked while a ${VAR} reference — which names a secret without being one —
// survives intact.
func TestRedactRecipeSecretsKeepsPlaceholdersMasksLiterals(t *testing.T) {
	fm, _, ok := splitFrontmatter(slackRecipe)
	if !ok {
		t.Fatalf("frontmatter did not split")
	}
	got := redactRecipeSecrets(fm)

	if !strings.Contains(got, `bot_token: "${SLACK_BOT_TOKEN}"`) {
		t.Errorf("placeholder credential was not preserved:\n%s", got)
	}
	if strings.Contains(got, "xapp-1-LITERAL-TOKEN") {
		t.Errorf("literal credential leaked into the recipe:\n%s", got)
	}
	if !strings.Contains(got, "app_token: <redacted>") {
		t.Errorf("literal credential was not masked:\n%s", got)
	}
	// Non-credential keys keep their values — masking them would make the
	// recipe useless for reading configuration.
	for _, want := range []string{"name: slack-bot", "model: claude-sonnet-5", `- "#rysh-test"`} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q to survive redaction:\n%s", want, got)
		}
	}
}

// TestRenderRecipeShowsFrontmatterAndPrompt verifies the rendered recipe carries
// the source path, the load status and both halves of the skill file.
func TestRenderRecipeShowsFrontmatterAndPrompt(t *testing.T) {
	var out strings.Builder
	renderRecipe(&out, "humanoids", "slack-bot",
		".rysh/humanoids/slack-bot/SKILL.md", true, slackRecipe, 70)
	got := out.String()

	for _, want := range []string{
		`recipe for "slack-bot"`,
		"source: .rysh/humanoids/slack-bot/SKILL.md",
		"status: loaded in this workspace",
		"description: Slack test humanoid",
		"You are a Slack assistant.",
		"Answer briefly.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("recipe missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "xapp-1-LITERAL-TOKEN") {
		t.Errorf("recipe leaked a literal credential:\n%s", got)
	}
}

// TestRenderRecipeInlineEntity: an entity spawned inline has no file, so the
// recipe is the system prompt the registry holds and says so.
func TestRenderRecipeInlineEntity(t *testing.T) {
	var out strings.Builder
	renderRecipe(&out, "agents", "scratch", "", true, "You review Go code.", 60)
	got := out.String()

	if !strings.Contains(got, "spawned inline") {
		t.Errorf("expected the inline-spawn note:\n%s", got)
	}
	if !strings.Contains(got, "You review Go code.") {
		t.Errorf("expected the registry system prompt:\n%s", got)
	}
}

// TestSkillFilePathResolvesToSkillMD covers the lookup `show` shares with
// spawn: an entity directory resolves to its SKILL.md, an explicit .md path
// passes through, and a name with nothing on disk still names the SKILL.md it
// was looking for (so the error points at the right file).
func TestSkillFilePathResolvesToSkillMD(t *testing.T) {
	t.Chdir(t.TempDir())
	writeSkill(t, "slack-bot", slackRecipe)

	if got, want := skillFilePath("humanoids", "slack-bot"),
		filepath.Join(".rysh", "humanoids", "slack-bot", "SKILL.md"); got != want {
		t.Errorf("entity dir: got %q, want %q", got, want)
	}

	legacy := filepath.Join(".rysh", "humanoids", "legacy.md")
	if err := os.WriteFile(legacy, []byte(slackRecipe), 0o644); err != nil {
		t.Fatalf("write legacy skill: %v", err)
	}
	if got := skillFilePath("humanoids", "./"+legacy); got != "./"+legacy {
		t.Errorf("explicit .md path: got %q, want %q", got, "./"+legacy)
	}

	if got, want := skillFilePath("humanoids", "nope"),
		filepath.Join(".rysh", "humanoids", "nope", "SKILL.md"); got != want {
		t.Errorf("missing entity: got %q, want %q", got, want)
	}
}

// TestShowReadsRecipeFromDisk exercises the read path `##humanoid show` uses:
// the file resolved by name is rendered verbatim, credentials aside.
func TestShowReadsRecipeFromDisk(t *testing.T) {
	t.Chdir(t.TempDir())
	writeSkill(t, "slack-bot", slackRecipe)

	path := skillFilePath("humanoids", "slack-bot")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recipe: %v", err)
	}
	def, err := parseHumanoidFile(path, envOnlyExpand)
	if err != nil {
		t.Fatalf("parse recipe: %v", err)
	}

	var out strings.Builder
	renderRecipe(&out, "humanoids", def.Name, path, false, string(content), 70)
	got := out.String()

	if !strings.Contains(got, `recipe for "slack-bot"`) {
		t.Errorf("recipe header missing the frontmatter name:\n%s", got)
	}
	if !strings.Contains(got, "status: not loaded in this workspace") {
		t.Errorf("expected the not-loaded status:\n%s", got)
	}
	if !strings.Contains(got, `bot_token: "${SLACK_BOT_TOKEN}"`) {
		t.Errorf("expected the ${VAR} reference unexpanded:\n%s", got)
	}
}
