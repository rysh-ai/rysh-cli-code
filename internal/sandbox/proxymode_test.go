// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// gap01 slice 2 (R9): `network: proxy` — the agent reaches its model through
// rysh's governance proxy and nothing else; the provider key stays host-side.

func TestProfileParsesNetworkProxy(t *testing.T) {
	dir := writeSandboxYAML(t, "profiles:\n  p: {network: proxy}\n")
	all, err := LoadProfiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := all["p"]
	if p.Network || !p.Proxy {
		t.Fatalf("network: proxy parsed as %+v", p)
	}
}

func TestMacOSNetworkProxyAllowsOnlyTheProxyPort(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, func(p *Profile, _ *Spec) { p.Network, p.Proxy = false, true })
	if err != nil {
		t.Fatal(err)
	}
	sb := plan.SBPL
	i := strings.Index(sb, "(deny network*)")
	j := strings.Index(sb, `(allow network-outbound (remote ip "localhost:1"))`)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("proxy mode must deny all network, then allow only the proxy port (deny=%d allow=%d):\n%s", i, j, sb)
	}
	if strings.Contains(sb, `(remote ip "*:*")`) {
		t.Error("proxy mode re-allows general network")
	}
}

func TestNetworkProxyNeedsTheGovernanceProxy(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	_, err := macPlan(t, l, agent, func(p *Profile, s *Spec) {
		p.Network, p.Proxy = false, true
		for i, kv := range s.Env {
			if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
				s.Env[i] = "ANTHROPIC_BASE_URL=https://api.example.invalid"
			}
		}
	})
	if err == nil || !strings.Contains(err.Error(), "governance proxy") {
		t.Fatalf("proxy mode without a loopback provider URL accepted: %v", err)
	}
}

func TestNetworkProxyKeepsTheKeyHostSide(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, func(p *Profile, s *Spec) {
		p.Network, p.Proxy = false, true
		s.Env = append(s.Env, "ANTHROPIC_API_KEY=synthetic-not-a-key", "CLAUDE_CODE_OAUTH_TOKEN=synthetic", "ANTHROPIC_AUTH_TOKEN=synthetic", "OPENAI_API_KEY=synthetic")
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range plan.Env {
		name, val, _ := strings.Cut(kv, "=")
		switch name {
		case "ANTHROPIC_API_KEY", "OPENAI_API_KEY":
			if val != config.ProxyDummyKey {
				t.Errorf("%s inside is not the proxy placeholder", name)
			}
		case "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN":
			t.Errorf("%s crossed into a proxy-mode sandbox", name)
		}
		if strings.Contains(val, "synthetic") {
			t.Errorf("a provider credential value crossed in %s", name)
		}
	}
	for _, a := range plan.Argv {
		if strings.Contains(a, "synthetic") {
			t.Error("a provider credential value is in argv")
		}
	}
}

func TestContainerNetworkProxyRefusedOnVMBackedDocker(t *testing.T) {
	p := builtinDefault()
	p.Backend, p.Image = BackendContainer, "local/x:1"
	p.Network, p.Proxy = false, true
	for _, goos := range []string{"darwin", "windows"} {
		if _, err := Select(p, goos, fakeLook("docker"), 1, 1); err == nil || !strings.Contains(err.Error(), "unix socket") {
			t.Errorf("%s: container + network: proxy accepted: %v", goos, err)
		}
	}
	if _, err := Select(p, "linux", fakeLook("docker"), 1, 1); err != nil {
		t.Errorf("linux: container + network: proxy refused: %v", err)
	}
}

func TestAutoOnLinuxWithProxyPrefersBwrap(t *testing.T) {
	p := builtinDefault()
	p.Network, p.Proxy = false, true
	b, err := Select(p, "linux", fakeLook("bwrap", "docker"), 1, 1)
	if err != nil || b.Name() != BackendBwrap {
		t.Fatalf("auto, linux, proxy = %v %v; want bwrap", b, err)
	}
}
