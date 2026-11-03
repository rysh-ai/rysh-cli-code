// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLook resolves only the tools in have, to absolute paths that need not
// exist: no stub binary is ever created, and nothing is put on PATH.
func fakeLook(have ...string) LookPathFunc {
	return func(name string) (string, error) {
		for _, h := range have {
			if h == name {
				return "/fake-tools/" + name, nil
			}
		}
		return "", errors.New("not found")
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// layout is a throwaway host: home, a project inside it, a rysh dir in the
// project, credential-shaped files in the home.
type layout struct {
	root, home, ws, rysh, sbxHome string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := layout{root: root, home: filepath.Join(root, "home")}
	l.ws = filepath.Join(l.home, "src", "proj")
	l.rysh = filepath.Join(l.ws, ".rysh")
	l.sbxHome = HomeDir(l.rysh, "default")
	for _, d := range []string{l.ws, l.rysh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(l.home, ".ssh", "id_synthetic"), "not a key")
	writeFile(t, filepath.Join(l.home, ".aws", "credentials"), "not a credential")
	writeFile(t, filepath.Join(l.home, ".rysh_env"), "export SYNTHETIC_NAME=1")
	writeFile(t, filepath.Join(l.home, ".claude", ".credentials.json"), "{}")
	writeFile(t, filepath.Join(l.home, ".claude", "CLAUDE.md"), "# instructions")
	writeFile(t, filepath.Join(l.rysh, "secrets", "x"), "synthetic")
	return l
}

func (l layout) spec(p Profile) Spec {
	return Spec{
		Profile: p, Workspace: l.ws, Home: l.sbxHome, HostHome: l.home, RyshDir: l.rysh,
		Argv: []string{"claude", "--session-id", "abc"},
		Env: []string{
			"PATH=/usr/bin:/bin", "TERM=xterm-256color", "HOME=" + l.home,
			"ANTHROPIC_BASE_URL=http://127.0.0.1:1", "SSH_AUTH_SOCK=/tmp/agent.sock",
			"CLAUDE_CONFIG_DIR=" + filepath.Join(l.home, ".claude"), "LANG=C",
		},
		ID:        "t1",
		ProxyPort: 1, // the session record's proxy port; the URL above names it
	}
}

func TestLoadProfilesMissingFileGivesDefault(t *testing.T) {
	all, err := LoadProfiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, ok := all[DefaultProfile]
	if !ok || d.Backend != BackendAuto || !d.Network {
		t.Fatalf("default = %+v ok=%v", d, ok)
	}
	if strings.Join(d.CopyConfig, ",") != "CLAUDE.md,commands,agents" {
		t.Fatalf("default copy_config = %v", d.CopyConfig)
	}
}

func TestLoadProfilesParses(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, FileName), `
profiles:
  offline:
    backend: container
    image: local/agent:1
    network: off
    read_only: [/opt/tools]
    copy_config: [CLAUDE.md, settings.json]
`)
	p, err := Resolve(dir, "offline")
	if err != nil {
		t.Fatal(err)
	}
	if p.Backend != BackendContainer || p.Image != "local/agent:1" || p.Network || len(p.ReadOnly) != 1 || len(p.CopyConfig) != 2 {
		t.Fatalf("got %+v", p)
	}
	if _, err := Resolve(dir, DefaultProfile); err != nil {
		t.Fatalf("built-in default must still exist: %v", err)
	}
}

func TestLoadProfilesFailsClosed(t *testing.T) {
	cases := map[string]string{
		"unknown key":      "profiles:\n  a:\n    backend: container\n    egress: [example.com]\n",
		"host allow-list":  "profiles:\n  a:\n    network: example.com\n",
		"bad backend":      "profiles:\n  a:\n    backend: vm\n",
		"relative ro path": "profiles:\n  a:\n    read_only: [tools]\n",
		"credential copy":  "profiles:\n  a:\n    copy_config: [.credentials.json]\n",
		"nested copy path": "profiles:\n  a:\n    copy_config: [../.ssh]\n",
		"bad name":         "profiles:\n  \"A B\":\n    backend: auto\n",
		"unknown top key":  "profile:\n  a: {}\n",
		"not yaml":         "profiles: [\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, FileName), body)
			if _, err := LoadProfiles(dir); err == nil {
				t.Fatalf("LoadProfiles accepted %q", body)
			}
		})
	}
}

func TestResolveUnknownProfile(t *testing.T) {
	_, err := Resolve(t.TempDir(), "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Resolve(t.TempDir(), "bad name;rm"); err == nil {
		t.Fatal("invalid name accepted")
	}
}

func TestSelect(t *testing.T) {
	p := builtinDefault()
	b, err := Select(p, "linux", fakeLook("docker"), 1, 1)
	if err != nil || b.Name() != BackendContainer {
		t.Fatalf("linux auto = %v, %v", b, err)
	}
	p.Backend = BackendMacOS
	if _, err := Select(p, "linux", fakeLook(), 1, 1); err == nil {
		t.Fatal("macos backend on linux must be refused, not substituted")
	}
}

func TestContainerAbsentFailsClosed(t *testing.T) {
	l := newLayout(t)
	p := builtinDefault()
	p.Backend, p.Image = BackendContainer, "local/agent:1"
	c := &Container{LookPath: fakeLook()}
	if err := c.Available(); err == nil {
		t.Fatal("Available with no docker/podman")
	}
	if _, err := c.Prepare(l.spec(p)); err == nil {
		t.Fatal("Prepare with no docker/podman must fail")
	}
}

func TestContainerPrefersDockerThenPodman(t *testing.T) {
	c := &Container{LookPath: fakeLook("podman", "docker")}
	if tool, _ := c.tool(); tool != "/fake-tools/docker" {
		t.Fatalf("tool = %s", tool)
	}
	c = &Container{LookPath: fakeLook("podman")}
	if tool, _ := c.tool(); tool != "/fake-tools/podman" {
		t.Fatalf("tool = %s", tool)
	}
}

func TestContainerNoImageFailsClosed(t *testing.T) {
	l := newLayout(t)
	p := builtinDefault()
	p.Backend = BackendContainer
	if _, err := (&Container{LookPath: fakeLook("docker")}).Prepare(l.spec(p)); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("err = %v", err)
	}
}

func containerPlan(t *testing.T, l layout, mut func(*Profile, *Spec)) *Plan {
	t.Helper()
	stdinIsTTY = func() bool { return true }
	t.Cleanup(func() { stdinIsTTY = realStdinIsTTY })
	p := builtinDefault()
	p.Backend, p.Image = BackendContainer, "local/agent:1"
	s := l.spec(p)
	if mut != nil {
		mut(&s.Profile, &s)
	}
	plan, err := (&Container{LookPath: fakeLook("docker"), UID: 501, GID: 20}).Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// TestContainerListsEnvAndMounts is the "no credential crosses" proof for the
// container backend: it lists every env NAME and every mount the sandbox gets.
func TestContainerListsEnvAndMounts(t *testing.T) {
	l := newLayout(t)
	plan := containerPlan(t, l, nil)
	t.Logf("inner env names: %v", plan.InnerEnvNames)
	for _, m := range plan.Mounts {
		t.Logf("mount: host=%s target=%s ro=%v hidden=%v", m.Host, m.Target, m.ReadOnly, m.Hidden)
	}

	want := "ANTHROPIC_BASE_URL,HOME,LANG,RYSH_SANDBOX,TERM"
	if got := strings.Join(plan.InnerEnvNames, ","); got != want {
		t.Fatalf("inner env names = %s, want %s", got, want)
	}
	visible := map[string]bool{}
	for _, m := range plan.Mounts {
		if !m.Hidden {
			visible[m.Host] = true
		}
		if m.Host != m.Target {
			t.Errorf("mount %s not at its own path (%s)", m.Host, m.Target)
		}
	}
	if !visible[l.ws] || !visible[l.sbxHome] || len(visible) != 2 {
		t.Fatalf("visible mounts = %v, want exactly workspace + sandbox home", visible)
	}
	for _, sp := range SensitivePaths(l.home) {
		for v := range visible {
			if within(v, sp) || within(sp, v) && v != l.ws {
				t.Errorf("visible mount %s overlaps sensitive %s", v, sp)
			}
		}
	}
	hiddenRysh := false
	for _, m := range plan.Mounts {
		if m.Hidden && m.Host == l.rysh {
			hiddenRysh = true
		}
	}
	if !hiddenRysh {
		t.Fatal("the rysh directory inside the workspace must be hidden")
	}

	argv := strings.Join(plan.Argv, " ")
	for _, want := range []string{
		"/fake-tools/docker run --rm -i -t", "--pull never", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--user 501:20", "--workdir " + l.ws,
		"local/agent:1 claude --session-id abc",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "--network") {
		t.Errorf("network on must not pass --network: %s", argv)
	}
	// Names only: no env VALUE may appear in argv.
	for _, v := range []string{"http://127.0.0.1:1", "/tmp/agent.sock", "xterm-256color"} {
		if strings.Contains(argv, v) {
			t.Errorf("argv carries an env value %q", v)
		}
	}
	// The tmpfs over .rysh comes before the deeper home bind.
	iR, iH := strings.Index(argv, "type=tmpfs,target="+l.rysh), strings.Index(argv, "source="+l.sbxHome)
	if iR < 0 || iH < 0 || iR > iH {
		t.Errorf("mount order wrong (rysh tmpfs %d, home bind %d)", iR, iH)
	}
	// The tmpfs over .rysh must be traversable (0711) by the non-root user, or
	// the sandbox home bind beneath it is unreachable (found by the gated
	// real-docker run: docker's tmpfs default is mode=700, owned by root).
	if !strings.Contains(argv, "type=tmpfs,target="+l.rysh+",tmpfs-mode=0711") {
		t.Errorf("rysh tmpfs not traversable: %s", argv)
	}
	if plan.Marker() != "container/default" {
		t.Errorf("marker = %s", plan.Marker())
	}
}

func TestContainerNetworkOff(t *testing.T) {
	l := newLayout(t)
	plan := containerPlan(t, l, func(p *Profile, _ *Spec) { p.Network = false })
	if !strings.Contains(strings.Join(plan.Argv, " "), "--network none") {
		t.Fatalf("argv = %v", plan.Argv)
	}
}

func TestContainerPathRules(t *testing.T) {
	l := newLayout(t)
	ro := filepath.Join(l.ws, "vendor")
	deny := filepath.Join(l.ws, "private")
	denyFile := filepath.Join(l.ws, "secret.txt")
	for _, d := range []string{ro, deny} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, denyFile, "x")
	plan := containerPlan(t, l, func(_ *Profile, s *Spec) {
		s.Rules = PathRules{ReadOnly: []string{ro}, Deny: []string{deny, denyFile}}
	})
	argv := strings.Join(plan.Argv, " ")
	for _, want := range []string{
		"type=bind,source=" + ro + ",target=" + ro + ",readonly",
		"type=tmpfs,target=" + deny,
		"type=bind,source=/dev/null,target=" + denyFile + ",readonly",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q", want)
		}
	}
}

func TestContainerRefusesSensitiveReadOnly(t *testing.T) {
	l := newLayout(t)
	p := builtinDefault()
	p.Backend, p.Image = BackendContainer, "local/agent:1"
	for _, ro := range []string{filepath.Join(l.home, ".ssh"), filepath.Join(l.home, ".aws"), l.home, "/"} {
		p.ReadOnly = []string{ro}
		if _, err := (&Container{LookPath: fakeLook("docker")}).Prepare(l.spec(p)); err == nil {
			t.Errorf("read_only %s accepted", ro)
		}
	}
}

func TestCheckSpecFailsClosed(t *testing.T) {
	l := newLayout(t)
	p := builtinDefault()
	cases := map[string]func(*Spec){
		"workspace is home":     func(s *Spec) { s.Workspace = l.home },
		"workspace above home":  func(s *Spec) { s.Workspace = l.root },
		"workspace in .ssh":     func(s *Spec) { s.Workspace = filepath.Join(l.home, ".ssh") },
		"workspace missing":     func(s *Spec) { s.Workspace = filepath.Join(l.ws, "nope") },
		"workspace relative":    func(s *Spec) { s.Workspace = "src/proj" },
		"workspace in rysh dir": func(s *Spec) { s.Workspace = l.rysh },
		"no argv":               func(s *Spec) { s.Argv = nil },
		"bad profile":           func(s *Spec) { s.Profile.Name = "x y" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s := l.spec(p)
			mut(&s)
			if err := CheckSpec(s); err == nil {
				t.Fatal("CheckSpec accepted it")
			}
		})
	}
	if err := CheckSpec(l.spec(p)); err != nil {
		t.Fatalf("good spec refused: %v", err)
	}
}

func TestPrepareHomeCopiesNoCredential(t *testing.T) {
	l := newLayout(t)
	hostCfg := filepath.Join(l.home, ".claude")
	writeFile(t, filepath.Join(hostCfg, "commands", "go.md"), "cmd")
	writeFile(t, filepath.Join(hostCfg, "commands", ".credentials.json"), "{}")
	if err := os.Symlink(filepath.Join(l.home, ".ssh"), filepath.Join(hostCfg, "commands", "ssh-link")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareHome(l.sbxHome, hostCfg, []string{"CLAUDE.md", "commands", "agents"}); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.Walk(l.sbxHome, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			rel, _ := filepath.Rel(l.sbxHome, p)
			got = append(got, rel)
		}
		return nil
	})
	t.Logf("sandbox home: %v", got)
	if strings.Join(got, ",") != ".,.claude,.claude/CLAUDE.md,.claude/commands,.claude/commands/go.md" {
		t.Fatalf("sandbox home = %v", got)
	}
	if fi, _ := os.Stat(l.sbxHome); fi.Mode().Perm() != 0o700 {
		t.Fatalf("home mode %v", fi.Mode().Perm())
	}
	if err := PrepareHome(l.sbxHome, hostCfg, []string{".credentials.json"}); err == nil {
		t.Fatal("credential copy accepted")
	}
}

func TestGitCommonDir(t *testing.T) {
	root := t.TempDir()
	common := filepath.Join(root, "repo", ".git")
	gitdir := filepath.Join(common, "worktrees", "wt")
	ws := filepath.Join(root, "wt")
	writeFile(t, filepath.Join(gitdir, "commondir"), "../..\n")
	writeFile(t, filepath.Join(ws, ".git"), "gitdir: "+gitdir+"\n")
	if got := GitCommonDir(ws); got != common {
		t.Fatalf("GitCommonDir = %s, want %s", got, common)
	}
	if got := GitCommonDir(root); got != "" {
		t.Fatalf("non-worktree = %q", got)
	}
}

// The docker CLIENT must keep the host HOME / DOCKER_* to find its context
// (colima, Docker Desktop); only the CONTAINER gets the sandbox home. Found by
// the gated real-docker run: with HOME=<sandbox home> the client dialled the
// default socket and failed.
func TestContainerToolKeepsHostHomeContainerGetsSandboxHome(t *testing.T) {
	l := newLayout(t)
	plan := containerPlan(t, l, func(_ *Profile, s *Spec) {
		s.Env = append(s.Env, "DOCKER_HOST=unix:///synthetic.sock", "DOCKER_CONTEXT=synthetic")
	})
	toolEnv := strings.Join(plan.Env, "\n") + "\n"
	for _, want := range []string{"HOME=" + l.home + "\n", "DOCKER_HOST=unix:///synthetic.sock\n", "DOCKER_CONTEXT=synthetic\n"} {
		if !strings.Contains(toolEnv, want) {
			t.Errorf("tool env missing %q", strings.TrimSpace(want))
		}
	}
	argv := strings.Join(plan.Argv, " ")
	if !strings.Contains(argv, "-e HOME="+l.sbxHome) || !strings.Contains(argv, "-e RYSH_SANDBOX=container/default") {
		t.Fatalf("container HOME/marker not explicit: %s", argv)
	}
	for _, n := range plan.InnerEnvNames {
		if strings.HasPrefix(n, "DOCKER_") {
			t.Errorf("%s crosses into the container", n)
		}
	}
}

func TestStdinTTYIsNotFooledByDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()
	if realStdinIsTTY() {
		t.Fatal("/dev/null read as a terminal: docker run -t would refuse to start")
	}
}
