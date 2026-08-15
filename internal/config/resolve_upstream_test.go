// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

// TestResolveUpstreamInheritsForgedAPIAllow verifies a workspace with its own
// [workspace.upstream] inherits the session-wide forged_api_allow/block unless it
// overrides them — so a top-level allow-list is not silently dropped.
func TestResolveUpstreamInheritsForgedAPIAllow(t *testing.T) {
	session := UpstreamConfig{
		Enabled:        true,
		URL:            "https://rysh.ai",
		APIKey:         "k",
		ForgedAPIAllow: []string{"weather_*"},
		ForgedAPIBlock: []string{"*_deleteAll"},
	}

	// Workspace overrides only connection details — should inherit the policy.
	wc := WorkspaceConfig{Name: "ws1", Upstream: &UpstreamConfig{Workspace: "ws-uuid"}}
	up := wc.ResolveUpstream(session)
	if len(up.ForgedAPIAllow) != 1 || up.ForgedAPIAllow[0] != "weather_*" {
		t.Fatalf("forged_api_allow not inherited: %v", up.ForgedAPIAllow)
	}
	if len(up.ForgedAPIBlock) != 1 || up.ForgedAPIBlock[0] != "*_deleteAll" {
		t.Fatalf("forged_api_block not inherited: %v", up.ForgedAPIBlock)
	}

	// Workspace sets its own allow — should win (no inheritance).
	wc2 := WorkspaceConfig{Name: "ws2", Upstream: &UpstreamConfig{ForgedAPIAllow: []string{"billing_*"}}}
	up2 := wc2.ResolveUpstream(session)
	if len(up2.ForgedAPIAllow) != 1 || up2.ForgedAPIAllow[0] != "billing_*" {
		t.Fatalf("workspace forged_api_allow should override: %v", up2.ForgedAPIAllow)
	}

	// No workspace upstream → uses the session as-is.
	up3 := WorkspaceConfig{Name: "ws3"}.ResolveUpstream(session)
	if len(up3.ForgedAPIAllow) != 1 || up3.ForgedAPIAllow[0] != "weather_*" {
		t.Fatalf("synthesized workspace should keep session allow: %v", up3.ForgedAPIAllow)
	}
}
