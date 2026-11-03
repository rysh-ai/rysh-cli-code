// SPDX-License-Identifier: Apache-2.0

package agentenv

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/sandbox"
)

// sandboxFixture is a throwaway host for `exec-clean --sandbox`: a home, a
// project inside it, and a rysh dir whose sandbox.yaml defines a container
// profile. No docker binary exists or is created — LookPath is faked to an
// absolute path that is never executed.
func sandboxFixture(t *testing.T, have ...string) (ws, rysh string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	ws = filepath.Join(home, "proj")
	rysh = filepath.Join(ws, ".rysh")
	if err := os.MkdirAll(rysh, 0o700); err != nil {
		t.Fatal(err)
	}
	yml := "profiles:\n  box:\n    backend: container\n    image: local/agent:1\n  noimg:\n    backend: container\n"
	if err := os.WriteFile(filepath.Join(rysh, "sandbox.yaml"), []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	// The session record: its governance proxy is on port 1, the port the
	// fixture env's ANTHROPIC_BASE_URL names (R12). The fixture env's
	// RYSH_SESSION names it: there is no fallback to "default".
	writeSessionRecord(t, rysh, "s1", `{"proxy_port":1}`)
	// The daemon publishes the policy fs rules; here: no fs section.
	if err := sandbox.WriteRules(rysh, sandbox.RulesFromFS(nil)); err != nil {
		t.Fatal(err)
	}
	old := sandboxDeps
	t.Cleanup(func() { sandboxDeps = old })
	sandboxDeps = sandboxEnv{
		GOOS:     "linux",
		HostHome: func() (string, error) { return home, nil },
		UID:      func() int { return 501 },
		GID:      func() int { return 20 },
		LookPath: func(name string) (string, error) {
			for _, h := range have {
				if h == name {
					return "/fake-tools/" + name, nil
				}
			}
			return "", errors.New("not found")
		},
	}
	return ws, rysh
}

type capturedExec struct {
	called bool
	path   string
	argv   []string
	env    []string
}

func (c *capturedExec) fn(path string, argv, env []string) error {
	c.called, c.path, c.argv, c.env = true, path, argv, env
	return nil
}

var sandboxEnvFixture = []string{
	"PATH=/usr/bin:/bin", "TERM=xterm", "ANTHROPIC_BASE_URL=http://127.0.0.1:1", "RYSH_SESSION=s1",
	"FAKE_SERVICE_TOKEN=x", "AWS_FAKE_NAME=x", "SSH_AUTH_SOCK=/tmp/s", "GH_TOKEN=x",
}

func TestMainSandboxExecsTheBackend(t *testing.T) {
	ws, rysh := sandboxFixture(t, "docker")
	var c capturedExec
	var stderr bytes.Buffer
	rc := Main([]string{"--agent", "claude", "--sandbox", "box", "--rysh-dir", rysh, "--workspace", ws, "--", "claude", "--session-id", "s1"},
		sandboxEnvFixture, &bytes.Buffer{}, &stderr, c.fn)
	if rc != 0 || !c.called {
		t.Fatalf("rc=%d called=%v stderr=%s", rc, c.called, stderr.String())
	}
	if c.path != "/fake-tools/docker" || c.argv[0] != "/fake-tools/docker" {
		t.Fatalf("exec path %s argv0 %s", c.path, c.argv[0])
	}
	argv := strings.Join(c.argv, " ")
	if !strings.Contains(argv, "local/agent:1 claude --session-id s1") {
		t.Fatalf("argv = %s", argv)
	}
	// The env-name listing: what the tool gets, and what crosses (-e NAME).
	var names []string
	for _, kv := range c.env {
		n, _, _ := strings.Cut(kv, "=")
		names = append(names, n)
	}
	t.Logf("tool env names: %v", names)
	for _, bad := range []string{"FAKE_SERVICE_TOKEN", "AWS_FAKE_NAME", "SSH_AUTH_SOCK", "GH_TOKEN"} {
		for _, n := range names {
			if n == bad {
				t.Errorf("%s reached the sandbox tool env", bad)
			}
		}
		if strings.Contains(argv, "-e "+bad) {
			t.Errorf("%s crosses into the container", bad)
		}
	}
	if !strings.Contains(argv, "-e ANTHROPIC_BASE_URL") || !strings.Contains(argv, "-e RYSH_SANDBOX") {
		t.Errorf("expected names missing: %s", argv)
	}
	if fi, err := os.Stat(filepath.Join(rysh, "sandbox", "homes", "box")); err != nil || !fi.IsDir() {
		t.Errorf("sandbox home not prepared: %v", err)
	}
}

func TestMainSandboxFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		have []string
		args func(ws, rysh string) []string
		rc   int
	}{
		{"unknown profile", []string{"docker"}, func(ws, r string) []string {
			return []string{"--sandbox", "nope", "--rysh-dir", r, "--workspace", ws, "--", "claude"}
		}, 125},
		{"no docker or podman", nil, func(ws, r string) []string {
			return []string{"--sandbox", "box", "--rysh-dir", r, "--workspace", ws, "--", "claude"}
		}, 125},
		{"no image", []string{"docker"}, func(ws, r string) []string {
			return []string{"--sandbox", "noimg", "--rysh-dir", r, "--workspace", ws, "--", "claude"}
		}, 125},
		{"missing workspace", []string{"docker"}, func(ws, r string) []string {
			return []string{"--sandbox", "box", "--rysh-dir", r, "--workspace", filepath.Join(ws, "gone"), "--", "claude"}
		}, 125},
		{"no --workspace", []string{"docker"}, func(ws, r string) []string {
			return []string{"--sandbox", "box", "--rysh-dir", r, "--", "claude"}
		}, 2},
		{"no --rysh-dir", []string{"docker"}, func(ws, r string) []string {
			return []string{"--sandbox", "box", "--workspace", ws, "--", "claude"}
		}, 2},
		// gap01 E3: codex is supported now; it fails closed the same way.
		{"codex unknown profile", []string{"docker"}, func(ws, r string) []string {
			return []string{"--agent", "codex", "--sandbox", "nope", "--rysh-dir", r, "--workspace", ws, "--", "codex"}
		}, 125},
		{"codex no docker or podman", nil, func(ws, r string) []string {
			return []string{"--agent", "codex", "--sandbox", "box", "--rysh-dir", r, "--workspace", ws, "--", "codex"}
		}, 125},
		{"path rules never published", []string{"docker"}, func(ws, r string) []string {
			_ = os.Remove(sandbox.RulesPath(r))
			return []string{"--sandbox", "box", "--rysh-dir", r, "--workspace", ws, "--", "claude"}
		}, 125},
		{"policy failed to load", []string{"docker"}, func(ws, r string) []string {
			_ = sandbox.WriteRules(r, sandbox.RulesFile{Version: 1, FailClosed: true})
			return []string{"--sandbox", "box", "--rysh-dir", r, "--workspace", ws, "--", "claude"}
		}, 125},
		{"wildcard deny rule", []string{"docker"}, func(ws, r string) []string {
			_ = sandbox.WriteRules(r, sandbox.RulesFile{Version: 1, Unenforceable: []string{"fs.deny **/.env"}})
			return []string{"--sandbox", "box", "--rysh-dir", r, "--workspace", ws, "--", "claude"}
		}, 125},
		{"macos backend not on linux", []string{"docker"}, func(ws, r string) []string {
			_ = os.WriteFile(filepath.Join(r, "sandbox.yaml"), []byte("profiles:\n  m:\n    backend: macos\n"), 0o600)
			return []string{"--sandbox", "m", "--rysh-dir", r, "--workspace", ws, "--", "claude"}
		}, 125},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws, rysh := sandboxFixture(t, tc.have...)
			var c capturedExec
			var stderr bytes.Buffer
			rc := Main(tc.args(ws, rysh), sandboxEnvFixture, &bytes.Buffer{}, &stderr, c.fn)
			if rc != tc.rc || c.called {
				t.Fatalf("rc=%d (want %d) exec called=%v stderr=%s", rc, tc.rc, c.called, stderr.String())
			}
			if rc == 125 && !strings.Contains(stderr.String(), "not started") {
				t.Errorf("stderr does not say the agent was not started: %s", stderr.String())
			}
		})
	}
}

// TestSandboxedTypedLineHasNoUnsandboxedBranch: the plain TypedLine falls back
// to `env -u … CMD` on a host without the rysh binary. A sandboxed line must
// not — that fallback would start the agent unsandboxed.
func TestSandboxedTypedLineHasNoUnsandboxedBranch(t *testing.T) {
	SetBinaryForTest("/opt/rysh/bin/rysh")
	line := SandboxedTypedLine(Claude, "box", "/w/.rysh", "/w", "claude --session-id s1")
	want := "test -x '/opt/rysh/bin/rysh' && '/opt/rysh/bin/rysh' exec-clean --agent claude --sandbox 'box' --rysh-dir '/w/.rysh' --workspace '/w' -- claude --session-id s1" +
		" || test -x '/opt/rysh/bin/rysh' || printf '%s\\n' 'rysh: cannot start the sandboxed agent: the rysh binary is missing on this host (the agent was not started)' >&2"
	if line != want {
		t.Fatalf("line =\n%s\nwant\n%s", line, want)
	}
	if strings.Contains(line, "env -u") {
		t.Fatal("sandboxed line carries the static fallback")
	}
	if strings.Count(line, "claude --session-id") != 1 {
		t.Fatal("the agent command appears outside the exec-clean branch")
	}
}

// TestMainSandboxAppliesPolicyRules: the published fs rules reach the backend.
func TestMainSandboxAppliesPolicyRules(t *testing.T) {
	ws, rysh := sandboxFixture(t, "docker")
	secret := filepath.Join(ws, ".env")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.WriteRules(rysh, sandbox.RulesFile{Version: 1, ReadOnly: []string{ws}, Deny: []string{secret}}); err != nil {
		t.Fatal(err)
	}
	var c capturedExec
	var stderr bytes.Buffer
	rc := Main([]string{"--agent", "claude", "--sandbox", "box", "--rysh-dir", rysh, "--workspace", ws, "--", "claude"},
		sandboxEnvFixture, &bytes.Buffer{}, &stderr, c.fn)
	if rc != 0 || !c.called {
		t.Fatalf("rc=%d stderr=%s", rc, stderr.String())
	}
	argv := strings.Join(c.argv, " ")
	for _, want := range []string{
		"type=bind,source=" + ws + ",target=" + ws + ",readonly",
		"type=bind,source=/dev/null,target=" + secret + ",readonly",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q", want)
		}
	}
}

// gap01 E3: codex through the same fail-closed path.
func TestMainSandboxCodex(t *testing.T) {
	ws, rysh := sandboxFixture(t, "docker")
	home := filepath.Dir(ws)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(home, ".codex", "AGENTS.md"), []byte("# a"), 0o600)
	var c capturedExec
	var stderr bytes.Buffer
	env := append(append([]string(nil), sandboxEnvFixture...), "OPENAI_BASE_URL=http://127.0.0.1:1/v1", "CODEX_HOME="+filepath.Join(home, ".codex"))
	rc := Main([]string{"--agent", "codex", "--sandbox", "box", "--rysh-dir", rysh, "--workspace", ws, "--", "codex"},
		env, &bytes.Buffer{}, &stderr, c.fn)
	if rc != 0 || !c.called {
		t.Fatalf("rc=%d called=%v stderr=%s", rc, c.called, stderr.String())
	}
	argv := strings.Join(c.argv, " ")
	sbxCodex := filepath.Join(rysh, "sandbox", "homes", "box", ".codex")
	if !strings.Contains(argv, "-e CODEX_HOME="+sbxCodex) || !strings.Contains(argv, "local/agent:1 codex") {
		t.Fatalf("argv = %s", argv)
	}
	if _, err := os.Stat(filepath.Join(sbxCodex, "auth.json")); err == nil {
		t.Error("codex auth.json copied into the sandbox home")
	}
	if _, err := os.Stat(filepath.Join(sbxCodex, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md not copied: %v", err)
	}
}

// LEAD-CHANGES (3): exec-clean hands the session bus ports to the sandbox.
func TestMainSandboxPassesBusPorts(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("uses the real macOS backend's Prepare (nothing is executed)")
	}
	ws, rysh := sandboxFixture(t, "docker")
	os.MkdirAll(filepath.Join(rysh, "sessions"), 0o700)
	os.WriteFile(filepath.Join(rysh, "sessions", "s.json"), []byte(`{"nats_port":1}`), 0o600)
	old := sandboxDeps
	defer func() { sandboxDeps = old }()
	sandboxDeps.GOOS = "darwin"
	var c capturedExec
	var stderr bytes.Buffer
	// macOS profile; ANTHROPIC_BASE_URL in the fixture is http://127.0.0.1:1 = the bus port.
	os.WriteFile(filepath.Join(rysh, "sandbox.yaml"), []byte("profiles:\n  m:\n    backend: macos\n"), 0o600)
	rc := Main([]string{"--sandbox", "m", "--rysh-dir", rysh, "--workspace", ws, "--", "/bin/sh"},
		sandboxEnvFixture, &bytes.Buffer{}, &stderr, c.fn)
	if rc != 125 || c.called || !strings.Contains(stderr.String(), "session bus") {
		t.Fatalf("rc=%d called=%v stderr=%s", rc, c.called, stderr.String())
	}
}

// gap01 slice 2 P2: exec-clean starts the proxy bridge for a network: proxy
// profile on a backend with its own netns, and refuses when it cannot.
func TestMainSandboxProxyStartsTheBridge(t *testing.T) {
	ws, rysh := sandboxFixture(t, "docker")
	os.WriteFile(filepath.Join(rysh, "sandbox.yaml"), []byte("profiles:\n  p:\n    backend: container\n    image: local/agent:1\n    network: proxy\n"), 0o600)
	var started []string
	old := sandboxDeps
	defer func() { sandboxDeps = old }()
	sandboxDeps.StartBridge = func(dir, upstream string, bus []int) (*sandbox.Bridge, error) {
		started = append(started, upstream)
		return &sandbox.Bridge{Dir: dir, Socket: filepath.Join(dir, "bridge.sock"), Binary: "/opt/rysh/bin/rysh", Port: 7777}, nil
	}
	writeSessionRecord(t, rysh, "s2", `{"proxy_port":7777}`)
	env := []string{"PATH=/usr/bin:/bin", "RYSH_SESSION=s2", "ANTHROPIC_BASE_URL=http://127.0.0.1:7777", "ANTHROPIC_API_KEY=synthetic"}
	var c capturedExec
	var stderr bytes.Buffer
	rc := Main([]string{"--sandbox", "p", "--rysh-dir", rysh, "--workspace", ws, "--", "claude"}, env, &bytes.Buffer{}, &stderr, c.fn)
	if rc != 0 || !c.called {
		t.Fatalf("rc=%d called=%v stderr=%s", rc, c.called, stderr.String())
	}
	if len(started) != 1 || started[0] != "127.0.0.1:7777" {
		t.Fatalf("bridge upstreams = %v", started)
	}
	argv := strings.Join(c.argv, " ")
	if !strings.Contains(argv, "sandbox-forward --listen 127.0.0.1:7777") || !strings.Contains(argv, "--network none") {
		t.Fatalf("argv = %s", argv)
	}
	if strings.Contains(argv, "synthetic") {
		t.Fatal("a key value is in argv")
	}

	sandboxDeps.StartBridge = func(string, string, []int) (*sandbox.Bridge, error) { return nil, errors.New("no bridge") }
	c = capturedExec{}
	stderr.Reset()
	rc = Main([]string{"--sandbox", "p", "--rysh-dir", rysh, "--workspace", ws, "--", "claude"}, env, &bytes.Buffer{}, &stderr, c.fn)
	if rc != 125 || c.called || !strings.Contains(stderr.String(), "not started") {
		t.Fatalf("bridge failure: rc=%d called=%v stderr=%s", rc, c.called, stderr.String())
	}
}

func writeSessionRecord(t *testing.T, rysh, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(rysh, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rysh, "sessions", name+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// R12: the bridge's upstream is the session record's proxy port. A provider
// URL naming any other loopback port (here the web UI's) is refused before a
// bridge is started, and the record of the pane's own session is the one read.
func TestMainSandboxProxyUpstreamIsTheSessionRecord(t *testing.T) {
	ws, rysh := sandboxFixture(t, "docker")
	os.WriteFile(filepath.Join(rysh, "sandbox.yaml"), []byte("profiles:\n  p:\n    backend: container\n    image: local/agent:1\n    network: proxy\n"), 0o600)
	writeSessionRecord(t, rysh, "work", `{"proxy_port":7777}`)
	var started []string
	old := sandboxDeps
	defer func() { sandboxDeps = old }()
	sandboxDeps.StartBridge = func(dir, upstream string, bus []int) (*sandbox.Bridge, error) {
		started = append(started, upstream)
		return &sandbox.Bridge{Dir: dir, Socket: filepath.Join(dir, "bridge.sock"), Binary: "/opt/rysh/bin/rysh", Port: 7777}, nil
	}
	run := func(url string) (int, string) {
		var c capturedExec
		var stderr bytes.Buffer
		env := []string{"PATH=/usr/bin:/bin", "RYSH_SESSION=work", "ANTHROPIC_BASE_URL=" + url}
		rc := Main([]string{"--sandbox", "p", "--rysh-dir", rysh, "--workspace", ws, "--", "claude"}, env, &bytes.Buffer{}, &stderr, c.fn)
		return rc, stderr.String()
	}
	if rc, out := run("http://127.0.0.1:7001"); rc != 125 || !strings.Contains(out, "not this session's governance proxy") || len(started) != 0 {
		t.Fatalf("a URL naming another loopback port: rc=%d started=%v stderr=%s", rc, started, out)
	}
	if rc, out := run("http://127.0.0.1:7777"); rc != 0 || len(started) != 1 || started[0] != "127.0.0.1:7777" {
		t.Fatalf("the session's proxy port: rc=%d started=%v stderr=%s", rc, started, out)
	}
}

// R9 round 3 (2): every credential name agentenv keeps for an agent is in the
// sandbox's proxy-mode drop list, minus the provider URLs and CODEX_HOME. A
// name added to a keep list fails here until the drop list has it.
func TestProxyDropListCoversEveryKeptCredential(t *testing.T) {
	drop := map[string]bool{}
	for _, n := range sandbox.ProxyDropNames() {
		drop[n] = true
	}
	notCreds := map[string]bool{"ANTHROPIC_BASE_URL": true, "OPENAI_BASE_URL": true, "CODEX_HOME": true}
	var lists [][]string
	lists = append(lists, keepClaude, keepClaudeBedrock, keepClaudeVertex, keepCodex)
	n := 0
	for _, l := range lists {
		for _, name := range l {
			if notCreds[name] {
				continue
			}
			n++
			if !drop[name] {
				t.Errorf("%s is kept for an agent but not dropped in network: proxy", name)
			}
		}
	}
	if n < 20 {
		t.Fatalf("checked only %d names; the keep lists moved", n)
	}
}
