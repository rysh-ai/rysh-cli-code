// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// gap01 E5: the Linux bubblewrap backend. Argv/profile tests here (they run
// on any OS: nothing is executed); the real bwrap run is bwrap_e2e_test.go,
// gated and Linux-only.

func bwrapPlan(t *testing.T, l layout, mut func(*Profile, *Spec)) (*Plan, error) {
	t.Helper()
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	p := builtinDefault()
	p.Backend = BackendBwrap
	s := l.spec(p)
	if mut != nil {
		mut(&s.Profile, &s)
	}
	b := &Bwrap{
		LookPath: fakeLook("bwrap"),
		ResolveAgent: func(name string, _ []string) (string, error) {
			if name == "claude" {
				return agent, nil
			}
			return "", errors.New("not found")
		},
	}
	return b.Prepare(s)
}

// argPairs reports whether argv holds the consecutive words want.
func argSeq(argv []string, want ...string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		ok := true
		for j, w := range want {
			if argv[i+j] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func TestBwrapAbsentFailsClosed(t *testing.T) {
	b := &Bwrap{LookPath: fakeLook()}
	if err := b.Available(); err == nil || !strings.Contains(err.Error(), "bwrap") {
		t.Fatalf("err = %v", err)
	}
	l := newLayout(t)
	p := builtinDefault()
	p.Backend = BackendBwrap
	if _, err := b.Prepare(l.spec(p)); err == nil {
		t.Fatal("Prepare without bwrap must fail")
	}
}

func TestBwrapArgvShape(t *testing.T) {
	l := newLayout(t)
	plan, err := bwrapPlan(t, l, func(p *Profile, _ *Spec) { p.Network = false })
	if err != nil {
		t.Fatal(err)
	}
	a := plan.Argv
	t.Logf("argv: %q", a)
	if a[0] != "/fake-tools/bwrap" {
		t.Fatalf("argv0 = %s", a[0])
	}
	for _, want := range [][]string{
		{"--die-with-parent"}, {"--new-session"}, {"--unshare-all"},
		{"--proc", "/proc"}, {"--dev", "/dev"}, {"--tmpfs", "/tmp"},
		{"--bind", l.ws, l.ws}, {"--bind", l.sbxHome, l.sbxHome},
		{"--tmpfs", l.rysh}, {"--chdir", l.ws},
	} {
		if !argSeq(a, want...) {
			t.Errorf("argv missing %q", want)
		}
	}
	// The agent runs as the last words, by absolute path.
	if !strings.HasSuffix(strings.Join(a, " "), "/opt/bin/claude --session-id abc") {
		t.Errorf("agent tail = %q", a[len(a)-4:])
	}
	// The rysh dir tmpfs comes before the sandbox-home bind beneath it.
	j := strings.Index(strings.Join(a, "\x00"), "--tmpfs\x00"+l.rysh)
	k := strings.Index(strings.Join(a, "\x00"), "--bind\x00"+l.sbxHome)
	if j < 0 || k < 0 || j > k {
		t.Errorf("rysh tmpfs (%d) must precede the home bind (%d)", j, k)
	}
	// Nothing of the real home is bound: no bind source under it except the
	// workspace, the sandbox home and the agent's install.
	for i := 0; i+2 < len(a); i++ {
		if a[i] != "--bind" && a[i] != "--ro-bind" && a[i] != "--ro-bind-try" {
			continue
		}
		src := a[i+1]
		if within(src, l.home) && !within(src, l.ws) && !within(src, l.sbxHome) {
			t.Errorf("a home path is bound: %s", src)
		}
		for _, sp := range SensitivePaths(l.home) {
			if within(src, sp) {
				t.Errorf("a sensitive path is bound: %s", src)
			}
		}
	}
	// No value ever in argv: the env goes by exec environment.
	for _, w := range a {
		if strings.Contains(w, "ANTHROPIC_BASE_URL") || strings.Contains(w, "SSH_AUTH_SOCK") {
			t.Errorf("env in argv: %s", w)
		}
	}
	for _, n := range plan.InnerEnvNames {
		if n == "SSH_AUTH_SOCK" || n == "CLAUDE_CONFIG_DIR" {
			t.Errorf("%s crosses into the sandbox", n)
		}
	}
}

func TestBwrapNetworkOff(t *testing.T) {
	l := newLayout(t)
	plan, err := bwrapPlan(t, l, func(p *Profile, _ *Spec) { p.Network = false })
	if err != nil {
		t.Fatal(err)
	}
	if argSeq(plan.Argv, "--share-net") {
		t.Fatal("network off still shares the net namespace")
	}
	if !argSeq(plan.Argv, "--unshare-all") {
		t.Fatal("no --unshare-all")
	}
}

func TestBwrapGitMetadataAndRules(t *testing.T) {
	l := newLayout(t)
	git := filepath.Join(l.ws, ".git")
	writeFile(t, filepath.Join(git, "config"), "[core]\n")
	vendor := filepath.Join(l.ws, "vendor")
	secret := filepath.Join(l.ws, "private")
	writeFile(t, filepath.Join(vendor, "x"), "x")
	writeFile(t, filepath.Join(secret, "y"), "y")
	plan, err := bwrapPlan(t, l, func(p *Profile, s *Spec) {
		p.Network = false
		s.Rules = PathRules{ReadOnly: []string{vendor}, Deny: []string{secret}}
	})
	if err != nil {
		t.Fatal(err)
	}
	a := plan.Argv
	for _, want := range [][]string{
		{"--bind", git, git},
		{"--ro-bind", filepath.Join(git, "hooks"), filepath.Join(git, "hooks")},
		{"--ro-bind", filepath.Join(git, "config"), filepath.Join(git, "config")},
		{"--ro-bind", vendor, vendor},
		{"--tmpfs", secret},
	} {
		if !argSeq(a, want...) {
			t.Errorf("argv missing %q", want)
		}
	}
}

func TestSelectBwrapOnLinux(t *testing.T) {
	p := builtinDefault()
	// LEAD-CHANGES (1): with network ON, bwrap shares the host's network
	// namespace (host loopback, the session bus), so auto never picks it then.
	b, err := Select(p, "linux", fakeLook("bwrap", "docker"), 1, 1)
	if err != nil || b.Name() != BackendContainer {
		t.Fatalf("auto on linux, network on = %v %v; want container", b, err)
	}
	p.Network = false
	b, err = Select(p, "linux", fakeLook("bwrap", "docker"), 1, 1)
	if err != nil || b.Name() != BackendBwrap {
		t.Fatalf("auto on linux with bwrap, network off = %v %v", b, err)
	}
	b, err = Select(p, "linux", fakeLook("docker"), 1, 1)
	if err != nil || b.Name() != BackendContainer {
		t.Fatalf("auto on linux without bwrap = %v %v", b, err)
	}
	p.Backend = BackendBwrap
	if _, err := Select(p, "darwin", fakeLook("bwrap"), 1, 1); err == nil {
		t.Fatal("bwrap on darwin accepted")
	}
	b, err = Select(p, "linux", fakeLook(), 1, 1)
	if err != nil || b.Name() != BackendBwrap {
		t.Fatalf("explicit bwrap must select bwrap (and fail at Available): %v %v", b, err)
	}
	if b.Available() == nil {
		t.Fatal("bwrap without the tool is available")
	}
}

func TestProfileAcceptsBwrap(t *testing.T) {
	dir := writeSandboxYAML(t, "profiles:\n  l: {backend: bwrap, network: off}\n")
	all, err := LoadProfiles(dir)
	if err != nil || all["l"].Backend != BackendBwrap {
		t.Fatalf("profiles = %+v err = %v", all, err)
	}
}

// LEAD-CHANGES (1): an explicit bwrap profile with network on is refused until
// slice 2 gives bwrap a network mode that does not share the host's.
func TestBwrapNetworkOnRefused(t *testing.T) {
	p := builtinDefault()
	p.Backend = BackendBwrap
	if _, err := Select(p, "linux", fakeLook("bwrap"), 1, 1); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("bwrap + network on accepted: %v", err)
	}
	l := newLayout(t)
	if _, err := bwrapPlan(t, l, nil); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("Prepare: bwrap + network on accepted: %v", err)
	}
}
