// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// E33 follow-up 2 (founder live test 2026-10-28, ~/e33-live-5): telegram
// connected and the founder's message reached the humanoid and was forwarded to
// its LLM, but no answer ever came. The assistant profile setup writes pins
// `model:`, so the humanoid builds its own provider config, and that config
// took its API key only from the child resolver (tab -> workspace): it dropped
// the session's configured key even for the SAME provider family, and the
// resolver never looked in the project scope (.rysh/secrets/default/) where
// setup stores ANTHROPIC_API_KEY. The model call went out with no key.

func TestE33HumanoidPinnedModelKeepsSessionKeySameFamily(t *testing.T) {
	e33ProjectDir(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	_ = os.Unsetenv("ANTHROPIC_API_KEY")

	w := e33Workspace()
	h := &HumanoidActor{
		name:          "assistant",
		provider:      "anthropic",
		providerModel: "claude-sonnet-5",
		cfg:           config.Config{ProviderName: "anthropic", APIKey: "session-config-key", APIURL: "http://127.0.0.1:1"},
		secrets:       w.childSecretResolver(),
	}
	cfg := h.providerOverrideConfig(providerFamily("anthropic"))
	if cfg.APIKey != "session-config-key" {
		t.Fatal("a same-family `model:` pin dropped the session's configured API key")
	}
	if cfg.APIURL != "http://127.0.0.1:1" {
		t.Fatal("a same-family `model:` pin dropped the session's configured API URL")
	}
	if cfg.DefaultModel != "claude-sonnet-5" {
		t.Fatalf("model = %q, want the pinned claude-sonnet-5", cfg.DefaultModel)
	}
}

func TestE33ChildResolverReadsProjectScope(t *testing.T) {
	e33ProjectDir(t)
	t.Setenv("OPENAI_API_KEY", "")
	_ = os.Unsetenv("OPENAI_API_KEY")
	e33WriteSecret(t, "default", "OPENAI_API_KEY", "project-openai-key")

	r := e33Workspace().childSecretResolver()
	if chain := r.scopeChain(""); len(chain) != 2 || chain[0] != e33Session || chain[1] != "default" {
		t.Fatalf("child resolver chain = %v, want [%s default]", chain, e33Session)
	}
	if got := r.providerAPIKey("", "openai"); got != "project-openai-key" {
		t.Fatal("a humanoid/pane in a named session did not find the key setup stored in .rysh/secrets/default/")
	}
}

// With an explicit workspaces: list, "default" can be another workspace's own
// scope, so no workspace falls back into it (provider_key_test's tenant rule).
func TestE33NamedWorkspacesDoNotFallBackToDefault(t *testing.T) {
	e33ProjectDir(t)
	e33WriteSecret(t, "default", e33TokenVar, "tenant-default-value")

	w := &WorkspaceActor{
		sessionName:   e33Session,
		workspaceName: "other",
		cfg:           config.Config{Workspaces: []config.WorkspaceConfig{{Name: "default"}, {Name: "other"}}},
		secrets:       newSecretStore(nil, nil),
	}
	if chain := w.secretScopeChain(""); len(chain) != 1 || chain[0] != "other" {
		t.Fatalf("workspace chain = %v, want [other]", chain)
	}
	if chain := w.childSecretResolver().scopeChain(""); len(chain) != 1 || chain[0] != "other" {
		t.Fatalf("child resolver chain = %v, want [other]", chain)
	}
	if got := w.namedExpandFunc("")(e33TokenRef); got != e33TokenRef {
		t.Fatal("workspace \"other\" resolved a value from workspace \"default\"'s scope")
	}
}
