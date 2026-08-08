package actors

// `##llm use <other-family>/<model>` must switch the PROVIDER, not just the
// model id.
//
// Session scope originally re-pinned the model on whatever provider the config
// built, which meant a cross-family selection sent (say) an OpenAI model id
// down the Anthropic client and 404'd on the next prompt. It was guarded off
// for exactly that reason; these tests cover the guard being replaced by the
// capability it was standing in for.

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// llmSwitchWorkspace is a workspace with a real SessionLLM seat, configured for
// the anthropic family.
func llmSwitchWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	return &WorkspaceActor{
		cfg: config.Config{
			RyshDir:      t.TempDir(),
			ProviderName: "claude",
			APIKey:       "config-anthropic-key",
		},
		agSetup: &agentic.Setup{SessionLLM: provider.NewSessionDefaults()},
	}
}

// TestLLMUse_CrossFamilySwitchesProvider: selecting an OpenAI model on an
// anthropic session installs an OpenAI provider in the session seat, and the
// seat's model is the registry entry's API id.
func TestLLMUse_CrossFamilySwitchesProvider(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	w := llmSwitchWorkspace(t)

	out := llmCmd(t, w, "add", "openai/sol", "gpt-5.6-sol")
	if !strings.Contains(out, "added openai/sol") {
		t.Fatalf("add: %s", out)
	}

	out = llmCmd(t, w, "use", "openai/sol")
	if !strings.Contains(out, "provider switched for this session: anthropic → openai") {
		t.Errorf("switch not reported:\n%s", out)
	}

	if model, _ := w.agSetup.SessionLLM.Get(); model != "gpt-5.6-sol" {
		t.Errorf("session model = %q, want gpt-5.6-sol", model)
	}
	prov := w.agSetup.SessionLLM.Provider()
	if prov == nil {
		t.Fatal("no session provider installed — the session would still call Anthropic")
	}
	if prov.Name() != "openai" {
		t.Errorf("session provider = %q, want openai", prov.Name())
	}
}

// TestLLMUse_SameFamilyKeepsConfiguredProvider: an anthropic model on an
// anthropic session must NOT build a new provider — that would drop the
// configured credentials and any provider-level configuration (thinking, etc.)
// for no reason.
func TestLLMUse_SameFamilyKeepsConfiguredProvider(t *testing.T) {
	w := llmSwitchWorkspace(t)

	out := llmCmd(t, w, "use", "anthropic/fable5")
	if strings.Contains(out, "provider switched") {
		t.Errorf("same-family selection must not switch provider:\n%s", out)
	}
	if model, _ := w.agSetup.SessionLLM.Get(); model != "claude-fable-5" {
		t.Errorf("session model = %q, want claude-fable-5", model)
	}
	if prov := w.agSetup.SessionLLM.Provider(); prov != nil {
		t.Errorf("same-family selection installed provider %q; it must keep the configured one", prov.Name())
	}
}

// TestLLMUse_SwitchBackDropsTheProvider: going back to the session's own family
// must RETRACT the cross-family provider, not leave it installed underneath a
// new model id — which would keep routing anthropic models to OpenAI.
func TestLLMUse_SwitchBackDropsTheProvider(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	w := llmSwitchWorkspace(t)

	llmCmd(t, w, "add", "openai/sol", "gpt-5.6-sol")
	llmCmd(t, w, "use", "openai/sol")
	if w.agSetup.SessionLLM.Provider() == nil {
		t.Fatal("precondition: expected a session provider after the cross-family switch")
	}

	llmCmd(t, w, "use", "anthropic/fable5")
	if prov := w.agSetup.SessionLLM.Provider(); prov != nil {
		t.Errorf("switching back left provider %q installed", prov.Name())
	}
}

// TestLLMClear_DropsTheProvider: ##llm clear returns the session to its
// configured provider as well as its configured model.
func TestLLMClear_DropsTheProvider(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")
	w := llmSwitchWorkspace(t)

	llmCmd(t, w, "add", "openai/sol", "gpt-5.6-sol")
	llmCmd(t, w, "use", "openai/sol")

	llmCmd(t, w, "clear")
	if model, _ := w.agSetup.SessionLLM.Get(); model != "" {
		t.Errorf("session model still %q after clear", model)
	}
	if prov := w.agSetup.SessionLLM.Provider(); prov != nil {
		t.Errorf("session provider %q survived clear", prov.Name())
	}
}

// TestLLMUse_CrossFamilyWarnsOnMissingKey: the configured provider's key
// belongs to the configured provider. A cross-family switch with no key for the
// new family must say so at selection time rather than 401 on the next prompt.
func TestLLMUse_CrossFamilyWarnsOnMissingKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	w := llmSwitchWorkspace(t)

	llmCmd(t, w, "add", "openai/sol", "gpt-5.6-sol")
	out := llmCmd(t, w, "use", "openai/sol")
	if !strings.Contains(out, "no API key found for openai") {
		t.Errorf("missing-key warning absent:\n%s", out)
	}
	if strings.Contains(out, "config-anthropic-key") {
		t.Fatal("the configured provider's key leaked into the output")
	}
}

// TestLLMUse_CrossFamilyKeyFromSecretStore: a key registered with ##secret must
// activate a cross-family selection.
//
// The switch resolved the new family's key from the daemon's environment ONLY,
// so `##secret` could list OPENAI_API_KEY while `##llm select` built an OpenAI
// provider with an empty key — the switch reported success, warned that no key
// existed, and every prompt afterwards failed to authenticate.
func TestLLMUse_CrossFamilyKeyFromSecretStore(t *testing.T) {
	t.Chdir(t.TempDir()) // isolate from any project-local .rysh/secrets
	t.Setenv("OPENAI_API_KEY", "")

	w := llmSwitchWorkspace(t)
	w.workspaceName = "ws-main"
	w.secrets = newSecretStore(nil, map[string]map[string]string{
		"ws-main": {"OPENAI_API_KEY": "sk-from-secret-store"},
	})

	if got := w.providerAPIKey("openai"); got != "sk-from-secret-store" {
		t.Fatalf("providerAPIKey = %q, want the ##secret-registered key", got)
	}

	llmCmd(t, w, "add", "openai/sol", "gpt-5.6-sol")
	out := llmCmd(t, w, "use", "openai/sol")
	if strings.Contains(out, "no API key found") {
		t.Errorf("a stored key was reported missing:\n%s", out)
	}
	if prov := w.agSetup.SessionLLM.Provider(); prov == nil || prov.Name() != "openai" {
		t.Fatalf("session provider not switched to openai: %v", prov)
	}
	if strings.Contains(out, "sk-from-secret-store") {
		t.Fatal("the resolved key leaked into the output")
	}
}

// TestProviderAPIKey_StoreBeatsEnvironment: the secret store is the primary
// tier, the environment the fallback — the same precedence every other rysh
// credential resolves with. A workspace with no store falls back to the
// environment unchanged (the pane/humanoid paths rely on that).
func TestProviderAPIKey_StoreBeatsEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-from-env")

	w := llmSwitchWorkspace(t)
	w.workspaceName = "ws-main"
	if got := w.providerAPIKey("openai"); got != "sk-from-env" {
		t.Fatalf("no store: got %q, want the environment value", got)
	}

	w.secrets = newSecretStore(nil, map[string]map[string]string{
		"ws-main": {"OPENAI_API_KEY": "sk-from-store"},
	})
	if got := w.providerAPIKey("openai"); got != "sk-from-store" {
		t.Fatalf("store tier: got %q, want sk-from-store", got)
	}

	// A family the store says nothing about still resolves from the environment.
	t.Setenv("GEMINI_API_KEY", "sk-gemini-env")
	if got := w.providerAPIKey("gemini"); got != "sk-gemini-env" {
		t.Fatalf("env fallback through the store: got %q, want sk-gemini-env", got)
	}
	// An empty variable is not a key: it must not shadow the next candidate name.
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "sk-google-env")
	if got := w.providerAPIKey("gemini"); got != "sk-google-env" {
		t.Fatalf("empty GEMINI_API_KEY shadowed GOOGLE_API_KEY: got %q", got)
	}
}
