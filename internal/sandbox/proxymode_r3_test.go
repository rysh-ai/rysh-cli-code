// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// LEAD-CHANGES round 3 on gap01 slice 2 (network: proxy).

// proxyCredEnv sets every name a provider credential arrives in (agentenv's
// keep lists, Bedrock and Vertex included) to a synthetic value.
var proxyCredEnv = []string{
	"ANTHROPIC_API_KEY=synthetic", "ANTHROPIC_AUTH_TOKEN=synthetic", "CLAUDE_CODE_OAUTH_TOKEN=synthetic",
	"CLAUDE_CODE_OAUTH_REFRESH_TOKEN=synthetic", "CLAUDE_CODE_SESSION_ACCESS_TOKEN=synthetic",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=synthetic", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=synthetic",
	"CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR=synthetic", "CLAUDE_CODE_CLIENT_KEY=synthetic",
	"ANTHROPIC_IDENTITY_TOKEN_FILE=synthetic", "ANTHROPIC_FOUNDRY_API_KEY=synthetic", "ANTHROPIC_AWS_API_KEY=synthetic",
	"AWS_ACCESS_KEY_ID=synthetic", "AWS_SECRET_ACCESS_KEY=synthetic", "AWS_SESSION_TOKEN=synthetic",
	"AWS_PROFILE=synthetic", "AWS_BEARER_TOKEN_BEDROCK=synthetic", "AWS_SHARED_CREDENTIALS_FILE=synthetic",
	"GOOGLE_APPLICATION_CREDENTIALS=synthetic", "OPENAI_API_KEY=synthetic",
}

func proxyMut(br *Bridge) func(*Profile, *Spec) {
	return func(p *Profile, s *Spec) {
		p.Network, p.Proxy = false, true
		s.Bridge = br
		s.Env = append(s.Env, proxyCredEnv...)
	}
}

// checkNoKey fails if a synthetic credential value is in env or argv, or the
// placeholder is missing.
func checkNoKey(t *testing.T, backend string, env, argv []string) {
	t.Helper()
	got := map[string]string{}
	for _, kv := range env {
		n, v, _ := strings.Cut(kv, "=")
		got[n] = v
		if strings.Contains(v, "synthetic") {
			t.Errorf("%s: a provider credential crossed in %s", backend, n)
		}
	}
	for _, n := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if got[n] != config.ProxyDummyKey {
			t.Errorf("%s: %s inside is not the proxy placeholder", backend, n)
		}
	}
	for _, a := range argv {
		if strings.Contains(a, "synthetic") {
			t.Errorf("%s: a provider credential value is in argv", backend)
		}
	}
}

// (1) R9 on Linux: bwrap and docker take the proxy-mode inner env too.
// (2) Every credential name agentenv keeps is dropped, not a hard-coded few.
func TestProxyModeKeepsEveryKeyHostSideOnEveryBackend(t *testing.T) {
	l := newLayout(t)
	br := &Bridge{Dir: l.root + "/run/abc", Socket: l.root + "/run/abc/bridge.sock", Binary: "/opt/rysh/bin/rysh", Port: 1}
	bp, err := bwrapPlan(t, l, proxyMut(br))
	if err != nil {
		t.Fatal(err)
	}
	checkNoKey(t, "bwrap", bp.Env, bp.Argv)
	cp := containerPlan(t, l, proxyMut(br))
	checkNoKey(t, "container", cp.Env, cp.Argv)
	mp, err := macPlan(t, l, installAgent(t, l.root+"/opt/bin"), proxyMut(nil))
	if err != nil {
		t.Fatal(err)
	}
	checkNoKey(t, "macos", mp.Env, mp.Argv)
}

// (2) With Bedrock or Vertex on, claude talks to AWS / Google, not to the
// governance proxy: proxy mode cannot serve it, and is refused.
func TestProxyModeRefusesBedrockAndVertex(t *testing.T) {
	for _, sw := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"} {
		l := newLayout(t)
		_, err := macPlan(t, l, installAgent(t, l.root+"/opt/bin"), func(p *Profile, s *Spec) {
			p.Network, p.Proxy = false, true
			s.Env = append(s.Env, sw+"=1")
		})
		if err == nil || !strings.Contains(err.Error(), sw) {
			t.Errorf("%s: proxy mode accepted: %v", sw, err)
		}
	}
}

// (3) The one loopback port is the session record's proxy port, never just
// whatever a provider URL names.
func TestLoopbackPortMustBeTheSessionProxyPort(t *testing.T) {
	setURL := func(s *Spec, u string) {
		for i, kv := range s.Env {
			if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
				s.Env[i] = "ANTHROPIC_BASE_URL=" + u
			}
		}
	}
	cases := map[string]func(*Profile, *Spec){
		"proxy mode, proxy off":            func(p *Profile, s *Spec) { p.Network, p.Proxy = false, true; s.ProxyPort = 0 },
		"proxy mode, URL names postgres":   func(p *Profile, s *Spec) { p.Network, p.Proxy = false, true; setURL(s, "http://127.0.0.1:5432") },
		"network on, URL names postgres":   func(_ *Profile, s *Spec) { setURL(s, "http://127.0.0.1:5432") },
		"network on, URL names the web UI": func(_ *Profile, s *Spec) { s.ProxyPort = 7000; setURL(s, "http://localhost:7001") },
		"network on, proxy off":            func(_ *Profile, s *Spec) { s.ProxyPort = 0 },
	}
	for name, mut := range cases {
		l := newLayout(t)
		plan, err := macPlan(t, l, installAgent(t, l.root+"/opt/bin"), mut)
		if err == nil {
			t.Errorf("%s: accepted; profile allows:\n%s", name, allowedLoopback(plan.SBPL))
		}
	}
	// The good case: the URL names the session's proxy port, and only it opens.
	l := newLayout(t)
	plan, err := macPlan(t, l, installAgent(t, l.root+"/opt/bin"), func(p *Profile, s *Spec) { p.Network, p.Proxy = false, true })
	if err != nil {
		t.Fatal(err)
	}
	if got := allowedLoopback(plan.SBPL); got != `(allow network-outbound (remote ip "localhost:1"))` {
		t.Errorf("loopback allows = %q", got)
	}
}

func allowedLoopback(sbpl string) string {
	var out []string
	for _, line := range strings.Split(sbpl, "\n") {
		if strings.Contains(line, `(allow network-outbound (remote ip "localhost:`) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// (4) The bridge dials, and the forwarder listens on, 127.0.0.1 only; a proxy
// URL on [::1] or localhost is refused rather than half-served.
func TestProxyModeNeedsLiteral127(t *testing.T) {
	for _, u := range []string{"http://[::1]:1", "http://localhost:1"} {
		l := newLayout(t)
		_, err := macPlan(t, l, installAgent(t, l.root+"/opt/bin"), func(p *Profile, s *Spec) {
			p.Network, p.Proxy = false, true
			for i, kv := range s.Env {
				if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
					s.Env[i] = "ANTHROPIC_BASE_URL=" + u
				}
			}
		})
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("%s: proxy mode accepted: %v", u, err)
		}
	}
}

// Round 4 (e20 LEAD-PASS fold-ins): SessionProxyPort reads only the pane's
// own record and fails closed (0, so the start is refused) on anything else.
// It never falls back to the "default" record and never reads
// RYSH_SESSION_DIR.
func TestSessionProxyPortFailsClosed(t *testing.T) {
	rysh := t.TempDir()
	sessions := filepath.Join(rysh, "sessions")
	writeFile(t, filepath.Join(sessions, "default.json"), `{"proxy_port": 7101}`)
	writeFile(t, filepath.Join(sessions, "good.json"), `{"proxy_port": 7102}`)
	writeFile(t, filepath.Join(sessions, "bad.json"), `{"proxy_port": `)
	writeFile(t, filepath.Join(sessions, "str.json"), `{"proxy_port": "7103"}`)
	writeFile(t, filepath.Join(sessions, "big.json"), `{"proxy_port": 70000}`)
	writeFile(t, filepath.Join(rysh, "escaped.json"), `{"proxy_port": 7104}`)
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "good.json"), `{"proxy_port": 7105}`)
	cases := map[string][]string{
		"no RYSH_SESSION":           nil,
		"empty RYSH_SESSION":        {"RYSH_SESSION="},
		"blank RYSH_SESSION":        {"RYSH_SESSION=  "},
		"missing record":            {"RYSH_SESSION=nosuch"},
		"malformed JSON":            {"RYSH_SESSION=bad"},
		"port as a string":          {"RYSH_SESSION=str"},
		"port out of range":         {"RYSH_SESSION=big"},
		"name ..":                   {"RYSH_SESSION=.."},
		"name .":                    {"RYSH_SESSION=."},
		"name with ../":             {"RYSH_SESSION=../escaped"},
		"RYSH_SESSION_DIR not read": {"RYSH_SESSION=nosuch", "RYSH_SESSION_DIR=" + other},
	}
	for name, env := range cases {
		if got := SessionProxyPort(rysh, env); got != 0 {
			t.Errorf("%s: SessionProxyPort = %d, want 0 (fail closed)", name, got)
		}
	}
	if got := SessionProxyPort(rysh, []string{"RYSH_SESSION=good", "RYSH_SESSION_DIR=" + other}); got != 7102 {
		t.Errorf("own record: SessionProxyPort = %d, want 7102", got)
	}
	if got := SessionProxyPort("", []string{"RYSH_SESSION=good"}); got != 0 {
		t.Errorf("no rysh dir: SessionProxyPort = %d, want 0", got)
	}
}
