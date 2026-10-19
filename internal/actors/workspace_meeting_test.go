// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/llms"
	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A meeting reads an agent's role from its skill file unexpanded: a ${VAR}
// that resolves to a secret for the agent's own runs must never reach a model
// that meeting participants can talk to.
func TestMeetingDefinitionIsUnexpandedAndCarriesVoiceAndModel(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MEETING_ROLE_TOKEN", "s3cret-value")
	writeAgentSkill(t, "scribe", `---
name: scribe
voice: nova
model: claude-opus-5-5
---
You are Scribe. Deploy token: ${MEETING_ROLE_TOKEN}.`)
	writeAgentSkill(t, "notes-dir", `---
name: planner
---
You plan.`)
	def, err := meetingDefinition("scribe", msg.AgentInfo{Name: "scribe", SystemPrompt: "expanded s3cret-value"})
	if err != nil || def.Voice != "nova" || def.Model != "claude-opus-5-5" || !strings.Contains(def.SystemPrompt, "${MEETING_ROLE_TOKEN}") || strings.Contains(def.SystemPrompt, "s3cret-value") {
		t.Fatalf("def %+v %v", def, err)
	}
	if def, _ := meetingDefinition("planner", msg.AgentInfo{Name: "planner"}); def.SystemPrompt != "You plan." {
		t.Fatalf("frontmatter name differing from its directory: %q", def.SystemPrompt)
	}
	// Inline agent: the typed prompt, which was never expanded.
	if def, _ := meetingDefinition("absent", msg.AgentInfo{Name: "absent", SystemPrompt: "typed"}); def.SystemPrompt != "typed" || def.Voice != "" {
		t.Fatal("inline agent role", def)
	}
}

// An agent spawned from an explicit path outside .rysh/agents is re-read from
// that path (recorded at spawn), never taken from its expanded prompt.
func TestMeetingDefinitionFollowsExplicitSpawnPath(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MEETING_ROLE_TOKEN", "s3cret-value")
	dir := t.TempDir()
	path := filepath.Join(dir, "reviewer.md")
	_ = os.WriteFile(path, []byte("---\nname: reviewer\nvoice: sage\n---\nReview with ${MEETING_ROLE_TOKEN}."), 0o600)
	loaded, err := parseSkillFile(path, envOnlyExpand)
	if err != nil || loaded.Path != path || !strings.Contains(loaded.SystemPrompt, "s3cret-value") {
		t.Fatalf("spawn parse %+v %v", loaded, err)
	}
	info := msg.AgentInfo{Name: "reviewer", SystemPrompt: loaded.SystemPrompt, SkillPath: loaded.Path}
	def, err := meetingDefinition("reviewer", info)
	if err != nil || def.Voice != "sage" || strings.Contains(def.SystemPrompt, "s3cret-value") {
		t.Fatalf("def %+v %v", def, err)
	}
	_ = os.Remove(path)
	if _, err := meetingDefinition("reviewer", info); err == nil {
		t.Fatal("fell back to the expanded prompt after the skill file vanished")
	}
}

type meetingFakeProvider struct{ name string }

func (p meetingFakeProvider) Name() string { return p.name }
func (meetingFakeProvider) Complete(context.Context, string) (string, error) {
	return "", errors.New("unused")
}
func (meetingFakeProvider) CompleteWithTools(context.Context, []sharedprovider.ConversationTurn, []sharedprovider.ToolSpec, string) (*sharedprovider.AgenticResponse, error) {
	return nil, errors.New("unused")
}

// `model:` applies only when the session's ##llm registry declares it,
// enabled and runnable with a key; otherwise the session model answers and
// the note says why.
func TestMeetingModelResolution(t *testing.T) {
	ryshDir := t.TempDir()
	store := llms.NewStore(ryshDir)
	for _, spec := range []llms.ModelSpec{
		{Provider: "anthropic", Name: "opus", Model: "claude-opus-5-5"},
		{Provider: "anthropic", Name: "parked", Model: "claude-parked", Disabled: true},
		{Provider: "openai", Name: "gpt", Model: "gpt-5"},
		{Provider: "mistral", Name: "large", Model: "mistral-large"},
	} {
		if _, err := store.Add(spec); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENAI_API_KEY", "")
	session := meetingFakeProvider{name: "session"}
	w := &WorkspaceActor{agSetup: &agentic.Setup{Provider: session}}
	w.cfg.RyshDir = ryshDir
	w.cfg.ProviderName = "anthropic"
	w.cfg.DefaultModel = "claude-sonnet-5"
	for model, want := range map[string][2]string{
		"":                {"session", ""},
		"claude-opus-5-5": {"anthropic/opus", ""},
		"anthropic/opus":  {"anthropic/opus", ""},
		"opus":            {"anthropic/opus", ""},
		"claude-unknown":  {"session", "not configured"},
		"parked":          {"session", "disabled"},
		"mistral/large":   {"session", "no executor"},
		"openai/gpt":      {"session", "OPENAI_API_KEY"},
	} {
		prov, label, note := w.meetingModel(model)
		if want[0] == "session" {
			if prov != sharedprovider.AgenticProvider(session) || !strings.Contains(label, "claude-sonnet-5") || !strings.Contains(note, want[1]) {
				t.Errorf("%q: %v %q %q", model, prov, label, note)
			}
			continue
		}
		if prov == sharedprovider.AgenticProvider(session) || label != want[0] || note != "" {
			t.Errorf("%q: %v %q %q", model, prov, label, note)
		}
	}
}

func TestMeetingAgentRefusesUnaddressableOrUnavailableAgents(t *testing.T) {
	w := &WorkspaceActor{}
	for _, name := range []string{"rysh", "agent2", "hey-bot"} {
		if _, err := w.meetingAgent(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if _, err := w.meetingAgent("scribe"); err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("agent without registry: %v", err)
	}
}

func TestAgentMeetingSubcommandNeedsLiveMeeting(t *testing.T) {
	w := &WorkspaceActor{}
	var out strings.Builder
	w.agentMeeting(&out, "p1", []string{"scribe"})
	if !strings.Contains(out.String(), "no active meeting") || w.ryshFail == nil {
		t.Fatalf("out %q", out.String())
	}
	w = &WorkspaceActor{meeting: meeting.NewManager()}
	out.Reset()
	w.agentMeeting(&out, "p1", []string{"scribe", "on"})
	if w.ryshFail == nil {
		t.Fatal("accepted a bad argument")
	}
}
