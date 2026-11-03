// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E-108 relay mode for the github channel: the humanoid fetches the
// workspace's GitHub connection from rysh-server to learn its ids, and copies
// NO secret down — the server holds the App's webhook secret and key.
func TestGitHubRelayFetchesTheConnectionAndNoSecrets(t *testing.T) {
	h, _, calls := humanoidAgainst(t, http.StatusOK,
		`{"connection_id":"conn-gh","workspace_id":"ws-uuid","connection_type":"github",`+
			`"config":{"installation_id":"77"},"secrets":{"webhook_secret":"S","app_private_key":"K","bot_token":"T","app_secret":"A"}}`)
	cc, refused := h.resolveChannelCredentialsFromUpstream("github", msg.ChannelConfig{Enabled: true, Relay: true, BotLogin: "ryshagent"})
	if refused != nil {
		t.Fatalf("refused: %v", refused)
	}
	if *calls != 1 {
		t.Fatalf("credentials fetched %d times, want 1", *calls)
	}
	if cc.RelayURL != h.cfg.Upstream.URL || cc.RelayAPIKey == "" || cc.RelayWorkspaceID != "ws-uuid" || cc.RelayConnectionID != "conn-gh" {
		t.Errorf("relay transport not wired: url %q key set %v ws %q conn %q", cc.RelayURL, cc.RelayAPIKey != "", cc.RelayWorkspaceID, cc.RelayConnectionID)
	}
	if cc.AppSecret != "" || cc.BotToken != "" || cc.AppToken != "" || cc.APIKey != "" {
		t.Errorf("a server-held secret was copied down in relay mode")
	}
}

// Direct mode keeps its own secret and token and fetches nothing.
func TestGitHubDirectModeFetchesNothing(t *testing.T) {
	h, _, calls := humanoidAgainst(t, http.StatusOK, `{}`)
	if _, err := h.resolveChannelCredentialsFromUpstream("github", msg.ChannelConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 {
		t.Errorf("direct-mode github fetched credentials %d times", *calls)
	}
}

// `mode: relay` is the documented alias for `relay: true`, as for WhatsApp;
// a typo fails loudly instead of silently running direct mode.
func TestGitHubModeRelayAlias(t *testing.T) {
	def, err := parseHumanoidFile(writeHumanoidSkillFile(t,
		"---\nname: reviewer\ncontacts:\n  github:\n    enabled: true\n    mode: relay\n    bot_login: ryshagent\n---\nbody\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !def.Contacts["github"].Relay {
		t.Fatal("mode: relay did not set Relay for github")
	}
	_, err = parseHumanoidFile(writeHumanoidSkillFile(t,
		"---\nname: reviewer\ncontacts:\n  github:\n    enabled: true\n    mode: rleay\n---\nbody\n"), nil)
	if err == nil || !strings.Contains(err.Error(), "github: unknown mode") {
		t.Fatalf("a mode typo: err = %v, want github: unknown mode", err)
	}
}
