// SPDX-License-Identifier: Apache-2.0

package actors

// A provider API key registered with ##secret must reach EVERY seat that builds
// a cross-family provider, not just the session one.
//
// The pane and humanoid override paths construct their provider inside their own
// actor, which had no secret store in reach and so resolved the key from the
// daemon's environment alone: `##secret` listed OPENAI_API_KEY while `##pane
// model openai/…` built a provider with an empty key. These cover the resolver
// the WorkspaceActor now hands down, and the two seats consuming it.

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

const (
	testTabScope = "tab-1111"
	testWSScope  = "ws-main"
)

// storeResolver builds a resolver over a config-tier store — the tier that
// needs no JetStream — scoped to testWSScope.
func storeResolver(t *testing.T, cfgSecrets map[string]map[string]string) *secretResolver {
	t.Helper()
	t.Chdir(t.TempDir()) // isolate from any project-local .rysh/secrets
	return &secretResolver{store: newSecretStore(nil, cfgSecrets), scope: testWSScope}
}

// TestSecretResolverScopeChain pins the chain the actors below a workspace
// resolve on: tab (most specific) then workspace, collapsing to the workspace
// alone when there is no distinct tab — which is every humanoid.
func TestSecretResolverScopeChain(t *testing.T) {
	r := &secretResolver{scope: testWSScope}

	if chain := r.scopeChain(testTabScope); len(chain) != 2 ||
		chain[0] != testTabScope || chain[1] != testWSScope {
		t.Fatalf("tab chain = %v, want [%s %s]", chain, testTabScope, testWSScope)
	}
	// No tab (a humanoid) resolves at the workspace scope only. An empty tabID
	// must NOT sanitize into the "default" scope and read another tenant's keys.
	if chain := r.scopeChain(""); len(chain) != 1 || chain[0] != testWSScope {
		t.Fatalf("no-tab chain = %v, want [%s]", chain, testWSScope)
	}
	// A tab whose token already equals the workspace scope is not repeated.
	if chain := r.scopeChain(testWSScope); len(chain) != 1 || chain[0] != testWSScope {
		t.Fatalf("collapsed chain = %v, want [%s]", chain, testWSScope)
	}
}

// TestSecretResolverPrecedence: tab beats workspace, workspace beats the
// environment, and a nil resolver — an actor constructed outside a workspace —
// still resolves from the environment rather than returning nothing.
func TestSecretResolverPrecedence(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-env")

	r := storeResolver(t, map[string]map[string]string{
		testWSScope:  {"OPENAI_API_KEY": "sk-workspace"},
		testTabScope: {"OPENAI_API_KEY": "sk-tab"},
	})

	if got := r.providerAPIKey(testTabScope, "openai"); got != "sk-tab" {
		t.Errorf("tab scope: got %q, want sk-tab", got)
	}
	if got := r.providerAPIKey("other-tab", "openai"); got != "sk-workspace" {
		t.Errorf("workspace fallback: got %q, want sk-workspace", got)
	}
	// A family the store says nothing about falls through to the environment.
	t.Setenv("GEMINI_API_KEY", "sk-gemini-env")
	if got := r.providerAPIKey(testTabScope, "gemini"); got != "sk-gemini-env" {
		t.Errorf("env tier: got %q, want sk-gemini-env", got)
	}

	var nilResolver *secretResolver
	if got := nilResolver.providerAPIKey(testTabScope, "openai"); got != "sk-from-env" {
		t.Errorf("nil resolver: got %q, want the environment value", got)
	}
}

// TestPaneProviderOverrideKeyFromSecretStore: a cross-family pane selection
// picks up a ##secret-registered key for its OWN tab, and never carries the
// configured provider's credentials into another family's requests.
func TestPaneProviderOverrideKeyFromSecretStore(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "") // the environment tier is empty

	p := &PaneActor{
		tabID: testTabScope,
		cfg:   config.Config{ProviderName: "claude", APIKey: "config-anthropic-key"},
		secrets: storeResolver(t, map[string]map[string]string{
			testTabScope: {"OPENAI_API_KEY": "sk-tab-openai"},
		}),
	}

	cfg := p.providerOverrideConfig("openai", "gpt-5.6-sol")
	if cfg.APIKey != "sk-tab-openai" {
		t.Errorf("APIKey = %q, want the ##secret-registered key", cfg.APIKey)
	}
	if cfg.ProviderName != "openai" || cfg.DefaultModel != "gpt-5.6-sol" {
		t.Errorf("provider/model = %q/%q, want openai/gpt-5.6-sol", cfg.ProviderName, cfg.DefaultModel)
	}

	// A SAME-family selection keeps the configured credentials and only re-pins
	// the model — it must not go looking for a key at all.
	same := p.providerOverrideConfig("anthropic", "claude-opus-4-8")
	if same.APIKey != "config-anthropic-key" {
		t.Errorf("same-family APIKey = %q, want the configured key", same.APIKey)
	}
	if same.DefaultModel != "claude-opus-4-8" {
		t.Errorf("same-family model = %q, want claude-opus-4-8", same.DefaultModel)
	}
}

// TestPaneProviderOverrideNoResolver: a pane with no workspace resolver behaves
// exactly as it did before the store was threaded down — environment-only.
func TestPaneProviderOverrideNoResolver(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-env")
	p := &PaneActor{tabID: testTabScope, cfg: config.Config{ProviderName: "claude"}}

	if got := p.providerOverrideConfig("openai", "").APIKey; got != "sk-from-env" {
		t.Errorf("APIKey = %q, want the environment value", got)
	}
}

// TestHumanoidProviderOverrideKeyFromSecretStore: a humanoid's `provider:`
// selection resolves at the WORKSPACE scope — it belongs to no tab, so a
// tab-scoped key must not reach it.
func TestHumanoidProviderOverrideKeyFromSecretStore(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	h := &HumanoidActor{
		name:          "bot",
		providerModel: "gpt-5.6-sol",
		cfg:           config.Config{ProviderName: "claude", APIKey: "config-anthropic-key"},
		secrets: storeResolver(t, map[string]map[string]string{
			testWSScope:  {"OPENAI_API_KEY": "sk-workspace-openai"},
			testTabScope: {"OPENAI_API_KEY": "sk-tab-openai"},
		}),
	}

	cfg := h.providerOverrideConfig("openai")
	if cfg.APIKey != "sk-workspace-openai" {
		t.Errorf("APIKey = %q, want the workspace-scoped key", cfg.APIKey)
	}
	if cfg.APIURL != "" {
		t.Errorf("APIURL = %q, want the selected family's own default", cfg.APIURL)
	}
}

// TestSecretResolverThreadedFromTabToPane walks the spawn chain the workspace
// hands its resolver down — tab → lane → stack → pane — because a single
// dropped argument anywhere along it silently reverts every pane to
// environment-only key resolution.
func TestSecretResolverThreadedFromTabToPane(t *testing.T) {
	res := &secretResolver{scope: testWSScope}
	// NewPaneActor reads its starting provider name off the setup.
	setup := &agentic.Setup{Provider: provider.NewAgenticProvider(config.Config{})}

	ta := NewTabActor("tab-1", "T", config.Config{}, nil, nil, setup, nil, res, nil)
	if ta.secrets != res {
		t.Fatal("tab dropped the resolver")
	}
	la := NewLaneActor("lane-1", ta.id, 10, "", "", ta.cfg, ta.pub, ta.nc, ta.agSetup, ta.kvStore, ta.secrets)
	if la.secrets != res {
		t.Fatal("lane dropped the resolver")
	}
	ga := NewPaneGroupActor("grp-1", la.tabID, la.id, "", "", la.cfg, la.pub, la.nc, la.agSetup, la.kvStore, la.secrets)
	if ga.secrets != res {
		t.Fatal("stack dropped the resolver")
	}
	pa := NewPaneActor("pane-1", "P", ga.tabID, ga.laneID, ga.id, ga.cfg, ga.pub, ga.nc, ga.agSetup, ga.kvStore, ga.secrets)
	if pa.secrets != res {
		t.Fatal("pane dropped the resolver")
	}
}
