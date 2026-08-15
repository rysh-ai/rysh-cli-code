// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// writeSkill creates .rysh/humanoids/<name>/SKILL.md under the current dir.
func writeSkill(t *testing.T, name, body string) {
	t.Helper()
	dir := filepath.Join(".rysh", "humanoids", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
}

// TestHumanoidArtefactDiscovery verifies parseHumanoidDir finds every
// <name>/SKILL.md artefact under .rysh/humanoids and extracts its name +
// channel contacts — the data the artefacts listing renders from.
func TestHumanoidArtefactDiscovery(t *testing.T) {
	t.Chdir(t.TempDir())

	writeSkill(t, "slack-bot", `---
name: slack-bot
description: Slack test humanoid
contacts:
  slack:
    bot_token: "${SLACK_BOT_TOKEN}"
    app_token: "${SLACK_APP_TOKEN}"
    channels:
      - "#rysh-test"
---
You are a Slack assistant.`)

	writeSkill(t, "email-bot", `---
name: email-bot
description: Email humanoid
contacts:
  email:
    type: gmail
    config:
      address: "a@b.com"
---
You are an email assistant.`)

	defs, err := parseHumanoidDir("", envOnlyExpand)
	if err != nil {
		t.Fatalf("parseHumanoidDir: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 artefacts, got %d", len(defs))
	}
	byName := map[string]*humanoidDefinition{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	if _, ok := byName["slack-bot"].Contacts["slack"]; !ok {
		t.Errorf("slack-bot should have a slack contact")
	}
	if _, ok := byName["email-bot"].Contacts["email"]; !ok {
		t.Errorf("email-bot should have an email contact")
	}
}

// TestRenderHumanoidArtefacts verifies the marking: loaded artefacts are tagged
// [loaded], others [not loaded], rows are name-sorted, and the summary counts
// the loaded ones.
func TestRenderHumanoidArtefacts(t *testing.T) {
	defs := []*humanoidDefinition{
		{
			Name:        "slack-bot",
			Description: "Slack test",
			Contacts:    map[string]msg.ChannelConfig{"slack": {Enabled: true}},
		},
		{Name: "email-bot", Description: "Email"},
	}

	loaded := map[string]bool{"slack-bot": true}

	var out strings.Builder
	renderHumanoidArtefacts(&out, ".rysh/humanoids", defs, loaded)
	s := out.String()

	if !strings.Contains(s, "2 artefact(s)") {
		t.Errorf("expected artefact count header, got:\n%s", s)
	}
	// email-bot sorts before slack-bot.
	if strings.Index(s, "email-bot") > strings.Index(s, "slack-bot") {
		t.Errorf("artefacts not name-sorted:\n%s", s)
	}
	if !strings.Contains(s, "slack-bot") || !strings.Contains(s, "[loaded") {
		t.Errorf("loaded artefact not marked [loaded]:\n%s", s)
	}
	if !strings.Contains(s, "[not loaded") {
		t.Errorf("unloaded artefact not marked [not loaded]:\n%s", s)
	}
	if !strings.Contains(s, "slack") {
		t.Errorf("expected slack channel type in output:\n%s", s)
	}
	if !strings.Contains(s, "1 of 2 loaded") {
		t.Errorf("expected loaded summary '1 of 2 loaded':\n%s", s)
	}
}
