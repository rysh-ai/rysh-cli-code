// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// security-lead ledger 220 (P-a, strict): a governance proxy that is REQUIRED
// (policy proxy_required, or strict) and cannot start must not leave panes
// running outside it: every pane shell refuses to start and says why. A proxy
// that is only enabled keeps today's behaviour (a logged error, panes start).
func TestRequiredProxyThatCannotStartBlocksPaneShells(t *testing.T) {
	t.Cleanup(func() { proxy.SetRequiredDown(""); proxyListenAddr = "" })
	proxyListenAddr = "256.256.256.256:1" // cannot be listened on

	cases := []struct {
		name   string
		cfg    config.ProxyConfig
		pol    *policy.Policy
		reason string // "" = panes may start
	}{
		{"strict", config.ProxyConfig{Enabled: true, Strict: true}, policy.Empty(), "proxy strict mode"},
		{"policy proxy_required", config.ProxyConfig{}, &policy.Policy{ProxyRequired: true}, "policy proxy_required"},
		{"enabled only", config.ProxyConfig{Enabled: true}, policy.Empty(), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proxy.SetRequiredDown("")
			w := &WorkspaceActor{cfg: config.Config{Proxy: c.cfg}, policy: c.pol}
			w.startProxy()
			if w.proxyServer != nil {
				t.Fatal("the proxy started on an address it cannot listen on")
			}
			got := paneShellBlockedByProxy()
			if c.reason == "" {
				if got != "" {
					t.Fatalf("an only-enabled proxy blocked pane shells: %q", got)
				}
				return
			}
			if !strings.Contains(got, c.reason) || !strings.Contains(got, "did not start") {
				t.Fatalf("pane shells are not blocked with the reason: %q (want %q)", got, c.reason)
			}
		})
	}

	// Once a required proxy runs, panes start again.
	proxyListenAddr = ""
	w := &WorkspaceActor{cfg: config.Config{Proxy: config.ProxyConfig{Enabled: true, Strict: true}}, policy: policy.Empty()}
	w.startProxy()
	t.Cleanup(func() {
		if w.proxyServer != nil {
			w.stopProxy()
		}
	})
	if w.proxyServer == nil {
		t.Fatal("the proxy did not start on the loopback default")
	}
	if got := paneShellBlockedByProxy(); got != "" {
		t.Fatalf("a running required proxy still blocks pane shells: %q", got)
	}
}
