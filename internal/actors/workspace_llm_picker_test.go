// SPDX-License-Identifier: Apache-2.0

package actors

// The picker payload is the whole contract between the daemon and every
// front-end that draws an arrow-key menu: the models, the scopes they can be
// bound at, and which providers have no key. Nothing in a front-end can
// recompute the last of those — only the daemon can see the ##secret store —
// so it is asserted here.

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/llms"
)

// pickerWorkspace is an anthropic-configured workspace whose registry holds one
// anthropic model and one openai model, with a ##secret-registered key for
// neither unless the test adds one.
func pickerWorkspace(t *testing.T) (*WorkspaceActor, *llms.Store) {
	t.Helper()
	t.Chdir(t.TempDir()) // isolate from any project-local .rysh/secrets
	w := &WorkspaceActor{
		cfg: config.Config{
			RyshDir:      t.TempDir(),
			ProviderName: "claude",
			APIKey:       "config-anthropic-key",
		},
		workspaceName: testWSScope,
	}
	store := llms.NewStore(w.cfg.RyshDir)
	if err := store.SeedIfEmpty(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.Add(llms.ModelSpec{Provider: "openai", Name: "luna", Model: "gpt-5.6-luna"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	return w, store
}

func findPickerModel(t *testing.T, w *WorkspaceActor, store *llms.Store, ref string) (found bool, keyMissing bool, keyName, model string) {
	t.Helper()
	selectable, blocked, err := w.llmSelectMenu(store)
	if err != nil {
		t.Fatalf("menu: %v", err)
	}
	payload := w.buildLLMPickerPayload("pane-1", store, selectable, blocked)
	for _, m := range payload.Models {
		if m.Ref == ref {
			return true, m.KeyMissing, m.KeyName, m.Model
		}
	}
	return false, false, "", ""
}

// TestPickerPayloadFlagsMissingKey: a cross-family model with no key anywhere
// is flagged, so the front-end knows to run the third step. This is the whole
// reason the flag is computed daemon-side.
func TestPickerPayloadFlagsMissingKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	w, store := pickerWorkspace(t)

	found, missing, keyName, model := findPickerModel(t, w, store, "openai/luna")
	if !found {
		t.Fatal("openai/luna missing from the payload")
	}
	if !missing {
		t.Error("openai/luna has no key anywhere but was not flagged")
	}
	if keyName != "OPENAI_API_KEY" {
		t.Errorf("KeyName = %q, want OPENAI_API_KEY", keyName)
	}
	if model != "gpt-5.6-luna" {
		t.Errorf("Model = %q, want the registry's api model id", model)
	}
}

// TestPickerPayloadSeesSecretStoreKey: a key registered with ##secret clears
// the flag. If it did not, the picker would demand a key the user already gave
// it — the same blindness that made `##llm select` fail in the first place.
func TestPickerPayloadSeesSecretStoreKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	w, store := pickerWorkspace(t)
	w.secrets = newSecretStore(nil, map[string]map[string]string{
		testWSScope: {"OPENAI_API_KEY": "sk-registered"},
	})

	if _, missing, _, _ := findPickerModel(t, w, store, "openai/luna"); missing {
		t.Error("a ##secret-registered key was reported missing")
	}
}

// TestPickerPayloadNeverFlagsTheConfiguredFamily: the session's own provider
// runs on cfg.APIKey, so it must never be reported keyless — nagging for
// ANTHROPIC_API_KEY on an anthropic session with a working key is noise.
func TestPickerPayloadNeverFlagsTheConfiguredFamily(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	w, store := pickerWorkspace(t)

	selectable, blocked, err := w.llmSelectMenu(store)
	if err != nil {
		t.Fatalf("menu: %v", err)
	}
	payload := w.buildLLMPickerPayload("pane-1", store, selectable, blocked)
	for _, m := range payload.Models {
		if m.Provider == "anthropic" && m.KeyMissing {
			t.Errorf("%s flagged keyless, but it runs on the configured key", m.Ref)
		}
	}
}

// TestPickerPayloadScopesCoverTheHierarchy: every level of the model hierarchy
// is offered, each with the command that binds it. A front-end appends the ref
// to Command verbatim, so a wrong or missing entry silently binds the wrong
// scope.
func TestPickerPayloadScopesCoverTheHierarchy(t *testing.T) {
	w, store := pickerWorkspace(t)
	selectable, blocked, _ := w.llmSelectMenu(store)
	payload := w.buildLLMPickerPayload("pane-1", store, selectable, blocked)

	want := []string{"session", "workspace", "tab", "lane", "stack", "pane"}
	if len(payload.Scopes) != len(want) {
		t.Fatalf("got %d scopes, want %d", len(payload.Scopes), len(want))
	}
	for i, name := range want {
		got := payload.Scopes[i]
		if got.Name != name {
			t.Errorf("scope %d = %q, want %q (broadest first)", i, got.Name, name)
		}
		if got.Command != "##"+name+" model" {
			t.Errorf("%s command = %q, want ##%s model", name, got.Command, name)
		}
		if strings.TrimSpace(got.Hint) == "" {
			t.Errorf("%s has no hint — the list is unreadable without one", name)
		}
	}
}

// TestPickerPayloadMarksTheModelInEffect so the front-end can open on it.
func TestPickerPayloadMarksTheModelInEffect(t *testing.T) {
	w, store := pickerWorkspace(t)
	w.sessionLLMRef = "openai/luna"

	selectable, blocked, _ := w.llmSelectMenu(store)
	payload := w.buildLLMPickerPayload("pane-1", store, selectable, blocked)
	if payload.InEffect != "openai/luna" {
		t.Errorf("InEffect = %q, want openai/luna", payload.InEffect)
	}
	current := 0
	for _, m := range payload.Models {
		if m.Current {
			current++
			if m.Ref != "openai/luna" {
				t.Errorf("Current marks %q", m.Ref)
			}
		}
	}
	if current != 1 {
		t.Errorf("%d models marked current, want exactly 1", current)
	}
}

// TestLLMSelectStillPrintsTheNumberedMenu: the push is additive. Removing the
// printed menu would strand the web UI, a detached session, and
// `rysh --llm select 3`, none of which listen for it.
func TestLLMSelectStillPrintsTheNumberedMenu(t *testing.T) {
	w, _ := pickerWorkspace(t)
	out := llmCmd(t, w, "select")
	if !strings.Contains(out, "select a model") || !strings.Contains(out, "##llm select <number>") {
		t.Errorf("numbered menu missing from a bare ##llm select:\n%s", out)
	}
}
