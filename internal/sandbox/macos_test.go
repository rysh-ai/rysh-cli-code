// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// macPlan prepares a macOS plan with the agent "installed" at agentPath (a real
// throwaway file — the backend reads its shebang, never runs it).
func macPlan(t *testing.T, l layout, agentPath string, mut func(*Profile, *Spec)) (*Plan, error) {
	t.Helper()
	p := builtinDefault()
	p.Backend = BackendMacOS
	s := l.spec(p)
	if mut != nil {
		mut(&s.Profile, &s)
	}
	m := &MacOS{
		ToolPath: "/fake-tools/sandbox-exec",
		ToolOK:   func(string) error { return nil },
		ResolveAgent: func(name string, _ []string) (string, error) {
			if name == "claude" {
				return agentPath, nil
			}
			return "", errors.New("not found")
		},
	}
	return m.Prepare(s)
}

func installAgent(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "claude")
	writeFile(t, p, "\x7fELF-not-a-script")
	return p
}

func TestMacOSAbsentFailsClosed(t *testing.T) {
	m := &MacOS{ToolPath: "/fake-tools/sandbox-exec", ToolOK: func(string) error { return errors.New("gone") }}
	if err := m.Available(); err == nil || !strings.Contains(err.Error(), "sandbox-exec") {
		t.Fatalf("err = %v", err)
	}
	l := newLayout(t)
	p := builtinDefault()
	p.Backend = BackendMacOS
	if _, err := m.Prepare(l.spec(p)); err == nil {
		t.Fatal("Prepare without sandbox-exec must fail")
	}
}

func TestMacOSProfileShape(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	sb := plan.SBPL
	t.Logf("generated profile:\n%s", sb)

	if plan.Argv[0] != "/fake-tools/sandbox-exec" || plan.Argv[1] != "-p" || plan.Argv[2] != sb || plan.Argv[3] != agent {
		t.Fatalf("argv head = %q", plan.Argv[:4])
	}
	if strings.Join(plan.Argv[4:], " ") != "--session-id abc" {
		t.Fatalf("agent args = %q", plan.Argv[4:])
	}
	// Order matters in SBPL: the last matching rule wins. Each always-deny must
	// come AFTER the allows it overrides.
	order := []string{
		"(version 1)",
		"(allow default)",
		"(deny file-write*)",
		`(allow file-write* (subpath "` + l.ws + `")`,
		`(deny file-read-data (subpath "` + l.home + `"))`,
		`(allow file-read-data (subpath "` + l.ws + `")`,
		`(deny file-read* file-read-data file-write* (subpath "` + l.rysh + `"))`,
		`(allow file-read* file-read-data file-write* (subpath "` + l.sbxHome + `"))`,
		`(deny file-read* file-read-data file-write* (subpath "` + filepath.Join(l.home, ".ssh") + `"))`,
		`(deny mach-lookup (global-name "com.apple.SecurityServer")`,
	}
	at := -1
	for _, want := range order {
		i := strings.Index(sb, want)
		if i < 0 {
			t.Fatalf("profile missing %q", want)
		}
		if i < at {
			t.Fatalf("profile rule %q out of order", want)
		}
		at = i
	}
	for _, sp := range []string{".ssh", ".aws", ".rysh_env", "Library/Keychains", ".claude", ".claude.json"} {
		if !strings.Contains(sb, `(subpath "`+filepath.Join(l.home, sp)+`")`) {
			t.Errorf("sensitive %s not denied", sp)
		}
	}
	if strings.Contains(sb, "(deny network*)") {
		t.Error("network on must not deny network")
	}
	// The env-name listing.
	t.Logf("inner env names: %v", plan.InnerEnvNames)
	if got := strings.Join(plan.InnerEnvNames, ","); got != "ANTHROPIC_BASE_URL,HOME,LANG,PATH,RYSH_SANDBOX,TERM" {
		t.Fatalf("inner env names = %s", got)
	}
	for _, kv := range plan.Env {
		if kv == "HOME="+l.sbxHome {
			goto homeOK
		}
	}
	t.Fatal("HOME is not the sandbox home")
homeOK:
	if plan.Marker() != "macos/default" {
		t.Fatalf("marker = %s", plan.Marker())
	}
}

func TestMacOSNetworkOffAndRules(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	ro := filepath.Join(l.ws, "vendor")
	deny := filepath.Join(l.ws, "private")
	plan, err := macPlan(t, l, agent, func(p *Profile, s *Spec) {
		p.Network = false
		s.Rules = PathRules{ReadOnly: []string{ro}, Deny: []string{deny}}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(deny network*)",
		`(deny file-write* (subpath "` + ro + `"))`,
		`(deny file-read* file-read-data file-write* (subpath "` + deny + `"))`,
	} {
		if !strings.Contains(plan.SBPL, want) {
			t.Errorf("profile missing %q", want)
		}
	}
}

func TestMacOSAgentInsideHomeIsAllowedNarrowly(t *testing.T) {
	l := newLayout(t)
	versions := filepath.Join(l.home, ".local", "share", "claude", "versions")
	real := installAgent(t, versions)
	link := filepath.Join(l.home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	plan, err := macPlan(t, l, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !allowsRead(plan.SBPL, versions) {
		t.Errorf("agent install dir not readable:\n%s", plan.SBPL)
	}
	if !strings.Contains(plan.SBPL, `(literal "`+link+`")`) {
		t.Errorf("agent link not readable")
	}
}

func TestMacOSScriptAgentAllowsItsInterpreterPrefix(t *testing.T) {
	l := newLayout(t)
	prefix := filepath.Join(l.home, ".nvm", "versions", "node", "v20")
	node := filepath.Join(prefix, "bin", "node")
	writeFile(t, node, "\x7fELF")
	if err := os.Chmod(node, 0o755); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(prefix, "lib", "node_modules", "claude-code", "cli.js")
	writeFile(t, cli, "#!/usr/bin/env node\nconsole.log(1)\n")
	p := builtinDefault()
	p.Backend = BackendMacOS
	s := l.spec(p)
	s.Env[0] = "PATH=" + filepath.Join(prefix, "bin") + ":/usr/bin" // replace the fixture's PATH
	m := &MacOS{
		ToolPath: "/fake-tools/sandbox-exec",
		ToolOK:   func(string) error { return nil },
		ResolveAgent: func(name string, env []string) (string, error) {
			if name == "claude" {
				return cli, nil
			}
			return resolveInPath(name, env)
		},
	}
	plan, err := m.Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	if !allowsRead(plan.SBPL, prefix) {
		t.Errorf("interpreter prefix not readable:\n%s", plan.SBPL)
	}
}

func TestMacOSRefusals(t *testing.T) {
	l := newLayout(t)
	cases := map[string]string{
		"agent at home root": installAgent(t, l.home),
		"agent in .ssh":      installAgent(t, filepath.Join(l.home, ".ssh")),
	}
	for name, agent := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := macPlan(t, l, agent, nil); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	t.Run("quote in path", func(t *testing.T) {
		agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
		bad := filepath.Join(l.ws, `we"ird`)
		if err := os.MkdirAll(bad, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := macPlan(t, l, agent, func(_ *Profile, s *Spec) { s.Rules.Deny = []string{bad} }); err == nil {
			t.Fatal("a path with a quote reached the profile")
		}
	})
	t.Run("agent not found", func(t *testing.T) {
		p := builtinDefault()
		p.Backend = BackendMacOS
		s := l.spec(p)
		s.Argv = []string{"nosuchagent"}
		m := &MacOS{ToolPath: "/x", ToolOK: func(string) error { return nil },
			ResolveAgent: func(string, []string) (string, error) { return "", errors.New("not found") }}
		if _, err := m.Prepare(s); err == nil {
			t.Fatal("accepted")
		}
	})
}

func TestSelectMacOSOnDarwin(t *testing.T) {
	b, err := Select(builtinDefault(), "darwin", fakeLook(), 1, 1)
	if err != nil || b.Name() != BackendMacOS {
		t.Fatalf("darwin auto = %v, %v", b, err)
	}
	p := builtinDefault()
	p.Backend = BackendContainer
	if b, err := Select(p, "darwin", fakeLook("docker"), 1, 1); err != nil || b.Name() != BackendContainer {
		t.Fatalf("darwin container = %v, %v", b, err)
	}
}

// allowsRead reports whether the profile's file-read-data allow rule names dir.
func allowsRead(sbpl, dir string) bool {
	for _, line := range strings.Split(sbpl, "\n") {
		if strings.HasPrefix(line, "(allow file-read-data ") && strings.Contains(line, `(subpath "`+dir+`")`) {
			return true
		}
	}
	return false
}
