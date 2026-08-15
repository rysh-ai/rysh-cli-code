// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// The gateway's newer controls have to survive the YAML boundary, because every
// one of them is off unless the file says otherwise — a key that silently fails
// to parse looks exactly like a feature nobody enabled.

func TestProxyStrictAndTenantKeysFromFile(t *testing.T) {
	writeConfig(t, `
proxy:
  enabled: true
  strict: true
  models:
    allow: ["claude-*", "gpt-4o"]
    deny: ["*-opus-*"]
  tenants:
    acme:
      panes: ["pane-1", "pane-2"]
      ceiling_tokens: 2000000
      keys:
        anthropic: acme-anthropic-key
        OpenAI: acme-openai-key
      rate_limit:
        rate: 3
        burst: 6
`)
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}

	if !cfg.Proxy.Strict {
		t.Error("proxy.strict did not reach the config (022 §8.2)")
	}

	acme, ok := cfg.Proxy.Tenants.Tenants["acme"]
	if !ok {
		t.Fatal("tenant acme missing")
	}
	if acme.CeilingTokens != 2_000_000 {
		t.Errorf("ceiling = %d, want 2000000", acme.CeilingTokens)
	}
	if acme.Keys["anthropic"] != "acme-anthropic-key" {
		t.Errorf("tenant anthropic key = %q", acme.Keys["anthropic"])
	}
	// Dialect names are normalised, so `OpenAI:` in a hand-written file is not
	// a key that silently never matches.
	if acme.Keys["openai"] != "acme-openai-key" {
		t.Errorf("tenant openai key = %q (dialect not lowercased?)", acme.Keys["openai"])
	}
	if acme.RateLimit.Rate != 3 || acme.RateLimit.Burst != 6 {
		t.Errorf("tenant rate rule = %+v", acme.RateLimit)
	}

	if got := cfg.Proxy.Models.Allow; len(got) != 2 || got[0] != "claude-*" {
		t.Errorf("model allowlist = %v", got)
	}
	if got := cfg.Proxy.Models.Deny; len(got) != 1 || got[0] != "*-opus-*" {
		t.Errorf("model denylist = %v", got)
	}
}

// TestProxyGatewayDefaults: every one of these is off unless asked for. A
// governance control that switches itself on during an upgrade breaks a session
// that used to work, which is the posture [proxy] enabled itself takes.
func TestProxyGatewayDefaults(t *testing.T) {
	cfg := applyDefaults()
	if cfg.Proxy.Strict {
		t.Error("proxy.strict must default off")
	}
	if len(cfg.Proxy.Models.Allow) != 0 || len(cfg.Proxy.Models.Deny) != 0 {
		t.Error("model rules must default to empty (every model allowed)")
	}
	if cfg.Upstream.Governance {
		t.Error("upstream.governance must default off — reporting spend to a " +
			"server is an explicit data-egress decision (023 §4.1)")
	}
}
