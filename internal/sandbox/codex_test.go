// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gap01 E3: codex runs sandboxed too. Its config dir is <sandbox home>/.codex,
// named by CODEX_HOME — a path, set explicitly; never the host's ~/.codex.

func TestSetEnvAllowList(t *testing.T) {
	l := newLayout(t)
	p := builtinDefault()
	p.Backend, p.Image = BackendContainer, "local/agent:1"
	s := l.spec(p)
	s.SetEnv = []string{"SOME_TOKEN=x"}
	if _, err := (&Container{LookPath: fakeLook("docker"), UID: 1, GID: 1}).Prepare(s); err == nil {
		t.Fatal("a SetEnv name outside the allow-list was accepted")
	}
}

func TestContainerCodexHome(t *testing.T) {
	l := newLayout(t)
	codexHome := filepath.Join(l.sbxHome, ".codex")
	plan := containerPlan(t, l, func(_ *Profile, s *Spec) {
		s.Argv = []string{"codex"}
		s.Env = append(s.Env, "CODEX_HOME="+filepath.Join(l.home, ".codex"))
		s.SetEnv = []string{"CODEX_HOME=" + codexHome}
	})
	argv := strings.Join(plan.Argv, " ")
	if !strings.Contains(argv, "-e CODEX_HOME="+codexHome) {
		t.Fatalf("CODEX_HOME not set to the sandbox home: %s", argv)
	}
	if strings.Contains(argv, filepath.Join(l.home, ".codex")+" ") || strings.Count(argv, "CODEX_HOME") != 1 {
		t.Fatalf("the host CODEX_HOME crossed: %s", argv)
	}
}

func TestMacOSCodexHome(t *testing.T) {
	l := newLayout(t)
	agent := filepath.Join(l.root, "opt", "bin", "codex")
	writeFile(t, agent, "\x7fELF")
	m := &MacOS{ToolPath: "/fake-tools/sandbox-exec", ToolOK: func(string) error { return nil },
		ResolveAgent: func(string, []string) (string, error) { return agent, nil }}
	p := builtinDefault()
	p.Backend = BackendMacOS
	s := l.spec(p)
	s.Argv = []string{"codex"}
	codexHome := filepath.Join(l.sbxHome, ".codex")
	s.SetEnv = []string{"CODEX_HOME=" + codexHome}
	plan, err := m.Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, kv := range plan.Env {
		found = found || kv == "CODEX_HOME="+codexHome
	}
	if !found {
		t.Fatalf("CODEX_HOME missing from %v", plan.InnerEnvNames)
	}
	// The host ~/.codex stays hidden.
	if !strings.Contains(plan.SBPL, `(subpath "`+filepath.Join(l.home, ".codex")+`")`) {
		t.Fatal("host ~/.codex not hidden")
	}
}

func TestPrepareHomeCodex(t *testing.T) {
	l := newLayout(t)
	host := filepath.Join(l.home, ".codex")
	writeFile(t, filepath.Join(host, "auth.json"), "{}")
	writeFile(t, filepath.Join(host, "AGENTS.md"), "# agents")
	writeFile(t, filepath.Join(host, "prompts", "p.md"), "p")
	writeFile(t, filepath.Join(host, "config.toml"), "model = 'x'")
	if err := PrepareHomeFor(l.sbxHome, host, ".codex", DefaultCopyConfigFor("codex")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(l.sbxHome, ".codex")
	for _, want := range []string{"AGENTS.md", "prompts/p.md"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("%s not copied: %v", want, err)
		}
	}
	for _, never := range []string{"auth.json", "config.toml"} {
		if _, err := os.Stat(filepath.Join(dst, never)); err == nil {
			t.Errorf("%s was copied into the sandbox", never)
		}
	}
}
