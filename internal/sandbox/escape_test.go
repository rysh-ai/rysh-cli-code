// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// escape_test.go — gap01 (G5). The ways out of a sandbox that are not a file
// read: the host's control plane (the session NATS on loopback, other
// processes' unix sockets), apps launched through LaunchServices, and code the
// agent leaves in the repository's git metadata for the user's next unsandboxed
// git command. Profile-text tests here; the real sandbox-exec runs are in
// escape_e2e_test.go.

func TestMacOSNetworkOnStillDeniesHostLoopbackExceptProxyPort(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, func(_ *Profile, s *Spec) {
		s.Env = append(s.Env, "OPENAI_BASE_URL=http://localhost:1/v1", "OTHER_BASE_URL=http://127.0.0.1:9999")
	})
	if err != nil {
		t.Fatal(err)
	}
	sb := plan.SBPL
	// The spec's ANTHROPIC_BASE_URL is http://127.0.0.1:1 (the governance proxy).
	for _, want := range []string{
		`(deny network-outbound (remote ip "localhost:*"))`,
		`(allow network-outbound (remote ip "localhost:1"))`,
		`(deny network-outbound (subpath "/"))`,
		`(allow network-outbound (literal "/private/var/run/mDNSResponder"))`,
	} {
		if !strings.Contains(sb, want) {
			t.Errorf("profile missing %q", want)
		}
	}
	// Only the agent's own provider URLs open a port; an arbitrary *_BASE_URL
	// does not.
	if strings.Contains(sb, "localhost:9999") {
		t.Error("an unrelated *_BASE_URL opened a loopback port")
	}
	// Order: the unix path deny, then TCP re-allowed, then loopback closed,
	// then the proxy port (the last matching rule wins).
	order := []string{
		`(deny network-outbound (subpath "/"))`,
		`(allow network-outbound (remote ip "*:*"))`,
		`(deny network-outbound (remote ip "localhost:*"))`,
		`(allow network-outbound (remote ip "localhost:1"))`,
	}
	last := -1
	for _, o := range order {
		i := strings.Index(sb, o)
		if i <= last {
			t.Errorf("%q out of order", o)
		}
		last = i
	}
	// The allow must come after the deny (the last matching rule wins).
	if strings.Index(sb, `(allow network-outbound (remote ip "localhost:1"))`) < strings.Index(sb, `(deny network-outbound (remote ip "localhost:*"))`) {
		t.Error("proxy-port allow precedes the loopback deny")
	}
	// A bare unix-socket filter matches all outbound traffic in SBPL.
	if strings.Contains(sb, "(remote unix-socket)") {
		t.Error("bare (remote unix-socket) would deny all network")
	}
	if strings.Contains(sb, "(deny network*)") {
		t.Error("network on must not deny all network")
	}
}

func TestMacOSRemoteProxyURLOpensNoLoopbackPort(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, func(_ *Profile, s *Spec) {
		for i, kv := range s.Env {
			if strings.HasPrefix(kv, "ANTHROPIC_BASE_URL=") {
				s.Env[i] = "ANTHROPIC_BASE_URL=https://api.example.invalid:8443"
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.SBPL, `(allow network-outbound (remote ip "localhost:`) {
		t.Errorf("a non-loopback base URL opened a loopback port:\n%s", plan.SBPL)
	}
}

func TestMacOSDeniesLaunchServicesAndAppleEvents(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range []string{"com.apple.coreservices.launchservicesd", "com.apple.coreservices.appleevents"} {
		if !strings.Contains(plan.SBPL, `(global-name "`+svc+`")`) {
			t.Errorf("profile does not deny mach service %s", svc)
		}
	}
}

func TestGitMetaPaths(t *testing.T) {
	ws := "/w/proj"
	got := mustList(t)(GitMetaPaths(ws, ""))
	want := []string{"/w/proj/.git/commondir", "/w/proj/.git/config", "/w/proj/.git/config.worktree", "/w/proj/.git/gitdir", "/w/proj/.git/hooks"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ordinary checkout: got %v want %v", got, want)
	}
	got = mustList(t)(GitMetaPaths(ws, "/r/main/.git"))
	for _, w := range []string{"/r/main/.git/config", "/r/main/.git/hooks", "/r/main/.git/config.worktree", "/w/proj/.git/config"} {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("linked worktree: %s missing from %v", w, got)
		}
	}
}

func TestMacOSGitMetadataIsReadOnly(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(l.ws, ".git")
	for _, want := range []string{
		`(deny file-write* (subpath "` + filepath.Join(git, "hooks") + `"))`,
		`(deny file-write* (literal "` + filepath.Join(git, "config") + `"))`,
		// .git itself cannot be renamed away (edit the config, rename back).
		`(deny file-write* (literal "` + git + `"))`,
	} {
		i := strings.Index(plan.SBPL, want)
		if i < 0 {
			t.Errorf("profile missing %q", want)
			continue
		}
		// After every write allow, including policy read_write carve-outs.
		if j := strings.LastIndex(plan.SBPL, "(allow file-write*"); j > i {
			t.Errorf("%q precedes a later write allow", want)
		}
	}
}

func TestMacOSGitMetadataStaysReadOnlyUnderPolicyCarveOut(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	git := filepath.Join(l.ws, ".git")
	plan, err := macPlan(t, l, agent, func(_ *Profile, s *Spec) {
		s.Rules = PathRules{ReadOnly: []string{l.ws}, ReadWrite: []string{git}}
	})
	if err != nil {
		t.Fatal(err)
	}
	hooks := `(deny file-write* (subpath "` + filepath.Join(git, "hooks") + `"))`
	carve := `(allow file-write* (subpath "` + git + `"))`
	if i, j := strings.Index(plan.SBPL, hooks), strings.Index(plan.SBPL, carve); i < 0 || j < 0 || i < j {
		t.Fatalf("hooks deny must follow the .git carve-out (hooks=%d carve=%d):\n%s", i, j, plan.SBPL)
	}
}

func TestContainerGitMetadataIsReadOnly(t *testing.T) {
	l := newLayout(t)
	git := filepath.Join(l.ws, ".git")
	writeFile(t, filepath.Join(git, "config"), "[core]\n")
	writeFile(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n")
	plan := containerPlan(t, l, nil)
	args := strings.Join(plan.Argv, " ")
	for _, want := range []string{
		// .git bound onto itself is a mount point: it cannot be renamed away
		// (EBUSY), so the read-only binds inside it stay where git looks.
		"type=bind,source=" + git + ",target=" + git + " ",
		"type=bind,source=" + filepath.Join(git, "hooks") + ",target=" + filepath.Join(git, "hooks") + ",readonly",
		"type=bind,source=" + filepath.Join(git, "config") + ",target=" + filepath.Join(git, "config") + ",readonly",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("argv missing %q", want)
		}
	}
	// The hooks dir is created (empty) so it can be held read-only: an agent
	// must not be able to create it inside the container.
	if fi, err := os.Stat(filepath.Join(git, "hooks")); err != nil || !fi.IsDir() {
		t.Errorf("hooks dir not prepared: %v", err)
	}
	// config.worktree is created empty and held read-only too, or the agent
	// could create it inside.
	cw := filepath.Join(git, "config.worktree")
	if fi, err := os.Stat(cw); err != nil || fi.Size() != 0 {
		t.Errorf("config.worktree not prepared empty: %v", err)
	}
	if !strings.Contains(args, "type=bind,source="+cw+",target="+cw+",readonly") {
		t.Error("config.worktree not bound read-only")
	}
}

func TestContainerNoGitDirAddsNoGitMounts(t *testing.T) {
	l := newLayout(t)
	plan := containerPlan(t, l, nil)
	if strings.Contains(strings.Join(plan.Argv, " "), "/.git/") {
		t.Errorf("git mounts without a .git dir: %v", plan.Argv)
	}
	if _, err := os.Stat(filepath.Join(l.ws, ".git")); err == nil {
		t.Error(".git created on the host")
	}
}

// LEAD-CHANGES (3): the provider port is the ONE loopback port a network-on
// macOS sandbox may reach. A provider URL that names the session bus port
// would reopen exactly what E1 closed, so it refuses the start.
func TestMacOSProviderPortThatIsTheBusRefuses(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	// The spec's ANTHROPIC_BASE_URL is http://127.0.0.1:1.
	_, err := macPlan(t, l, agent, func(_ *Profile, s *Spec) { s.BusPorts = []int{4222, 1} })
	if err == nil || !strings.Contains(err.Error(), "session bus") {
		t.Fatalf("provider port = bus port accepted: %v", err)
	}
	if _, err := macPlan(t, l, agent, func(_ *Profile, s *Spec) { s.BusPorts = []int{4222} }); err != nil {
		t.Fatalf("an unrelated bus port refused the start: %v", err)
	}
}

func TestBusPorts(t *testing.T) {
	rysh := t.TempDir()
	writeFile(t, filepath.Join(rysh, "sessions", "a.json"), `{"name":"a","nats_port":24973}`)
	writeFile(t, filepath.Join(rysh, "sessions", "b.json"), `{"name":"b"}`)
	writeFile(t, filepath.Join(rysh, "sessions", "junk.json"), `{{{`)
	got := BusPorts(rysh, []string{"RYSH_NATS_PORT=5000", "PATH=/bin"})
	want := []int{4222, 5000, 24973}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
