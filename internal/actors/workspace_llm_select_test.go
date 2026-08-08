package actors

// `##llm select` is a two-step picker: print a numbered menu, then take the
// number. The contract worth pinning is that a number the user can SEE is a
// number that WORKS — anything unactivatable is listed without one — and that
// selection reuses the ##llm use path rather than duplicating its guards.

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

func llmSelectWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	return &WorkspaceActor{
		cfg: config.Config{
			RyshDir:      t.TempDir(),
			ProviderName: "claude",
			APIKey:       "config-key",
		},
		agSetup: &agentic.Setup{SessionLLM: provider.NewSessionDefaults()},
	}
}

// TestLLMSelect_MenuNumbersOnlyActivatableModels: grok ships in the seed as a
// declaration with no rysh executor. It must appear (nothing vanishes from the
// registry view) but must NOT consume a number, or the numbering would offer
// choices that are refused the moment they are picked.
func TestLLMSelect_MenuNumbersOnlyActivatableModels(t *testing.T) {
	w := llmSelectWorkspace(t)
	out := llmCmd(t, w, "select")

	if !strings.Contains(out, "select a model") {
		t.Fatalf("no menu header:\n%s", out)
	}
	if !strings.Contains(out, "grok/grok-4") {
		t.Error("a non-executable model vanished from the menu instead of being shown as blocked")
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "grok/") && strings.Contains(line, ")") {
			t.Errorf("grok was given a menu number, but selecting it would be refused:\n    %s", line)
		}
	}
	if !strings.Contains(out, "no rysh executor for provider grok") {
		t.Error("blocked entry does not say WHY it cannot be picked")
	}
	if !strings.Contains(out, "##llm select <number>") {
		t.Error("menu does not say how to choose")
	}
}

// TestLLMSelect_DisabledModelIsBlockedNotNumbered: same rule for a model the
// user disabled — shown, with the command to re-enable it, but not numbered.
func TestLLMSelect_DisabledModelIsBlockedNotNumbered(t *testing.T) {
	w := llmSelectWorkspace(t)
	llmCmd(t, w, "disable", "anthropic/fable5")

	out := llmCmd(t, w, "select")
	if !strings.Contains(out, "##llm enable anthropic/fable5") {
		t.Errorf("disabled model missing its re-enable hint:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "anthropic/fable5") && strings.Contains(line, ")") {
			t.Errorf("a disabled model was numbered:\n    %s", line)
		}
	}
}

// TestLLMSelect_NumberActivates: picking a number runs the same activation the
// ##llm use path does — here the setup gap is what proves it got that far.
func TestLLMSelect_NumberActivates(t *testing.T) {
	w := llmSelectWorkspace(t)
	menu := llmCmd(t, w, "select")

	// Find what "1)" points at, then select it and confirm the session moved
	// to that exact model.
	var first string
	for _, line := range strings.Split(menu, "\n") {
		if strings.Contains(line, " 1) ") {
			fields := strings.Fields(line)
			first = fields[len(fields)-2] // "<ref> <model-id>"
			break
		}
	}
	if first == "" {
		t.Fatalf("no numbered entry in the menu:\n%s", menu)
	}

	out := llmCmd(t, w, "select", "1")
	if !strings.Contains(out, "session default set: "+first) {
		t.Errorf("selecting 1 did not activate %q:\n%s", first, out)
	}
}

// TestLLMSelect_RejectsBadInput: out-of-range and non-numeric input must say
// what to do next rather than failing silently or picking something.
func TestLLMSelect_RejectsBadInput(t *testing.T) {
	w := llmSelectWorkspace(t)

	if out := llmCmd(t, w, "select", "999"); !strings.Contains(out, "out of range") {
		t.Errorf("out-of-range not reported:\n%s", out)
	}
	if out := llmCmd(t, w, "select", "0"); !strings.Contains(out, "out of range") {
		t.Errorf("zero is not a valid 1-based choice:\n%s", out)
	}
	if out := llmCmd(t, w, "select", "banana"); !strings.Contains(out, "not a menu number") {
		t.Errorf("non-numeric input not reported:\n%s", out)
	}
}

// TestLLMSelect_RefFallsThroughToUse: `##llm select openai/sol` is an easy slip
// and unambiguous, so it activates rather than complaining about the format.
func TestLLMSelect_RefFallsThroughToUse(t *testing.T) {
	w := llmSelectWorkspace(t)
	out := llmCmd(t, w, "select", "anthropic/fable5")
	if !strings.Contains(out, "session default set: anthropic/fable5") {
		t.Errorf("a provider/name argument was not treated as an activation:\n%s", out)
	}
}

// TestLLMSelect_MarksTheModelInEffect: the menu has to answer "which one am I
// on?" or the user cannot tell what selecting will change.
func TestLLMSelect_MarksTheModelInEffect(t *testing.T) {
	w := llmSelectWorkspace(t)
	llmCmd(t, w, "use", "anthropic/sonnet-5")

	out := llmCmd(t, w, "select")
	if !strings.Contains(out, "in effect now: anthropic/sonnet-5") {
		t.Errorf("menu does not name the active model:\n%s", out)
	}
	marked := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "anthropic/sonnet-5") && strings.HasPrefix(strings.TrimRight(line, " "), "  >") {
			marked = true
		}
	}
	if !marked {
		t.Errorf("active model not marked with > in the list:\n%s", out)
	}
}

// TestLLMSelect_Aliases: pick/menu reach the same command, so muscle memory
// from either word works.
func TestLLMSelect_Aliases(t *testing.T) {
	w := llmSelectWorkspace(t)
	for _, verb := range []string{"select", "pick", "menu"} {
		if out := llmCmd(t, w, verb); !strings.Contains(out, "select a model") {
			t.Errorf("##llm %s did not open the menu:\n%s", verb, out)
		}
	}
}
