package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// llmCmd runs one ##llm invocation against a fresh registry rooted in a temp
// rysh dir and returns the rendered output.
func llmCmd(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handleLLMCommand(&out, "", args)
	return out.String()
}

func newLLMTestWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	return &WorkspaceActor{cfg: config.Config{RyshDir: t.TempDir()}}
}

// TestLLMCommand_Subcommands pins the ##llm surface: list (default), add,
// use, info — plus the model-add alias and the clear/unknown paths.
func TestLLMCommand_Subcommands(t *testing.T) {
	w := newLLMTestWorkspace(t)

	// Bare ##llm lists the seeded registry (all four seed providers).
	out := llmCmd(t, w)
	for _, want := range []string{"anthropic", "openai", "grok", "gemini", "fable5"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}

	// add persists a new declaration; list shows it; info reads it back.
	out = llmCmd(t, w, "add", "openai/gpt55")
	if !strings.Contains(out, "added openai/gpt55") {
		t.Errorf("add output: %s", out)
	}
	if out = llmCmd(t, w, "list"); !strings.Contains(out, "gpt55") {
		t.Errorf("list after add missing gpt55: %s", out)
	}
	out = llmCmd(t, w, "info", "openai/gpt55")
	for _, want := range []string{"model id    : gpt55", "executable  : yes", "file        :"} {
		if !strings.Contains(out, want) {
			t.Errorf("info output missing %q:\n%s", want, out)
		}
	}

	// A provider rysh has no executor for reports so — grok is the only one
	// left, now that the executor speaks the OpenAI-compatible dialects.
	if out = llmCmd(t, w, "info", "grok/grok-4"); !strings.Contains(out, "executable  : no") {
		t.Errorf("info on a non-executable provider:\n%s", out)
	}

	// model add stays as a back-compat alias.
	if out = llmCmd(t, w, "model", "add", "openai/gpt66", "gpt-6.6"); !strings.Contains(out, "added openai/gpt66") {
		t.Errorf("model add alias output: %s", out)
	}
	if out = llmCmd(t, w, "info", "openai/gpt66"); !strings.Contains(out, "model id    : gpt-6.6") {
		t.Errorf("info after aliased add: %s", out)
	}

	// info on an unknown ref points at add.
	if out = llmCmd(t, w, "info", "openai/nope"); !strings.Contains(out, "##llm add openai/nope") {
		t.Errorf("info unknown output: %s", out)
	}

	// use on a model whose provider has no executor refuses activation but
	// keeps the entry. This used to be every non-anthropic model, which meant
	// ##llm refused openai/gemini models that ##pane model would happily run.
	if out = llmCmd(t, w, "use", "grok/grok-4"); !strings.Contains(out, "NOT activatable") {
		t.Errorf("use on a non-executable provider: %s", out)
	}

	// Without an agentic setup there is nowhere to install a session default,
	// and that must be said rather than silently swallowed — for a same-family
	// and a cross-family selection alike. (The switch itself is exercised in
	// TestLLMUse_CrossFamilySwitchesProvider below.)
	for _, ref := range []string{"anthropic/fable5", "openai/gpt55"} {
		if out = llmCmd(t, w, "use", ref); !strings.Contains(out, "agentic setup unavailable") {
			t.Errorf("use %s without setup output: %s", ref, out)
		}
	}

	// Unknown bare word → usage listing all subcommands.
	out = llmCmd(t, w, "bogus")
	for _, want := range []string{"##llm add", "##llm use", "##llm info", "##llm status", "##llm | ##llm list"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q:\n%s", want, out)
		}
	}
}

// TestLLMCommand_Status pins ##llm status (and its `default` alias): the
// no-override state names the config/built-in default, and the loop seats
// line reflects the CONFIG-EFFECTIVE models, not the raw built-ins.
func TestLLMCommand_Status(t *testing.T) {
	w := newLLMTestWorkspace(t)

	out := llmCmd(t, w, "status")
	for _, want := range []string{"session default :", "claude-opus-4-8 (built-in)", "no ##llm override",
		"loop seats", "do/step " + "claude-sonnet-5", "judge " + "claude-opus-4-8", "precedence"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}

	// `default` alias renders the same header.
	if out = llmCmd(t, w, "default"); !strings.Contains(out, "session default :") {
		t.Errorf("default alias output:\n%s", out)
	}

	// Config-effective seats show through (not the built-ins).
	w.cfg.Automation.Web.Step = &webauto.StepConfig{Model: "claude-haiku-4-5-20251001"}
	w.cfg.Automation.Web.Loop = &webauto.LoopConfig{While: &webauto.WhileConfig{Model: "claude-fable-5"}}
	out = llmCmd(t, w, "status")
	if !strings.Contains(out, "do/step claude-haiku-4-5-20251001") || !strings.Contains(out, "judge claude-fable-5") {
		t.Errorf("status seats not config-effective:\n%s", out)
	}

	// With a config-file default model, the label switches source.
	w.cfg.DefaultModel = "claude-sonnet-5"
	if out = llmCmd(t, w, "status"); !strings.Contains(out, "claude-sonnet-5 (rysh.config.yaml)") {
		t.Errorf("status config-default label:\n%s", out)
	}
}

// TestLLMCommand_EnableDisable pins the enable/disable surface: disabling
// leaves the entry listed but marked and refuses activation, and re-enabling
// restores it.
func TestLLMCommand_EnableDisable(t *testing.T) {
	w := newLLMTestWorkspace(t)
	llmCmd(t, w) // seed

	if out := llmCmd(t, w, "disable", "anthropic/fable5"); !strings.Contains(out, "anthropic/fable5 disabled") {
		t.Errorf("disable output: %s", out)
	}

	// Still listed, now marked.
	out := llmCmd(t, w, "list")
	if !strings.Contains(out, "fable5") || !strings.Contains(out, "[disabled]") {
		t.Errorf("list after disable:\n%s", out)
	}
	if out = llmCmd(t, w, "info", "anthropic/fable5"); !strings.Contains(out, "enabled     : no") {
		t.Errorf("info after disable:\n%s", out)
	}

	// Activation is refused — and the refusal names the way back.
	if out = llmCmd(t, w, "use", "anthropic/fable5"); !strings.Contains(out, "##llm enable anthropic/fable5") {
		t.Errorf("use disabled output: %s", out)
	}

	// The flag survives a round-trip through the registry file: re-enabling
	// restores activatability (here: the agSetup gap, not the disabled gate).
	if out = llmCmd(t, w, "enable", "anthropic/fable5"); !strings.Contains(out, "anthropic/fable5 enabled") {
		t.Errorf("enable output: %s", out)
	}
	if out = llmCmd(t, w, "use", "anthropic/fable5"); !strings.Contains(out, "agentic setup unavailable") {
		t.Errorf("use after enable: %s", out)
	}
	if out = llmCmd(t, w, "list"); strings.Contains(out, "[disabled]") {
		t.Errorf("list still marks disabled after enable:\n%s", out)
	}

	// Unknown ref points at add; a bare word without a slash reports usage.
	if out = llmCmd(t, w, "disable", "anthropic/nope"); !strings.Contains(out, "##llm add anthropic/nope") {
		t.Errorf("disable unknown output: %s", out)
	}
	if out = llmCmd(t, w, "disable"); !strings.Contains(out, "usage: ##llm disable") {
		t.Errorf("disable without ref: %s", out)
	}
}

// TestLLMCommand_DisableActiveDefault pins the safety rule: disabling the
// model currently in effect must not leave the session running on it.
func TestLLMCommand_DisableActiveDefault(t *testing.T) {
	w := newLLMTestWorkspace(t)
	w.agSetup = &agentic.Setup{SessionLLM: provider.NewSessionDefaults()}
	llmCmd(t, w) // seed

	if out := llmCmd(t, w, "use", "anthropic/fable5"); !strings.Contains(out, "session default set") {
		t.Fatalf("use output: %s", out)
	}
	if model, _ := w.agSetup.SessionLLM.Get(); model != "claude-fable-5" {
		t.Fatalf("session default not installed: %q", model)
	}

	out := llmCmd(t, w, "disable", "anthropic/fable5")
	if !strings.Contains(out, "it was the session default") {
		t.Errorf("disable active default output:\n%s", out)
	}
	if model, effort := w.agSetup.SessionLLM.Get(); model != "" || effort != "" {
		t.Errorf("session default still installed after disabling it: %q/%q", model, effort)
	}
	if w.sessionLLMRef != "" {
		t.Errorf("sessionLLMRef not cleared: %q", w.sessionLLMRef)
	}
}
