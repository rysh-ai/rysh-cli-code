// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// escape_e2e_test.go — gap01 (G5): the escape suite under the REAL
// /usr/bin/sandbox-exec. ENV-GATED like the other real-backend tests
// (RYSH_SANDBOX_E2E=1, macOS only; a skip in the default suite). Throwaway dirs
// only; the "agent" is /bin/sh.
//
// Every connection in here goes to a listener THIS TEST opened on loopback or
// a unix socket in its own temp dir. Nothing leaves the host: no DNS, no
// outside address, no session NATS.

// macRun runs script under the generated profile and reports its error.
func macRun(t *testing.T, l layout, mut func(*Spec), script string) error {
	t.Helper()
	p := builtinDefault()
	p.Backend = BackendMacOS
	s := l.spec(p)
	s.Argv = []string{"/bin/sh", "-c", script}
	tmp := filepath.Join(l.root, "agent-tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	s.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + tmp}
	if mut != nil {
		mut(&s)
	}
	plan, err := (&MacOS{}).Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
	cmd.Env = plan.Env
	cmd.Dir = l.ws
	out, err := cmd.CombinedOutput()
	t.Logf("%q -> err=%v out=%s", script, err, strings.TrimSpace(string(out)))
	return err
}

func gateMacE2E(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || os.Getenv("RYSH_SANDBOX_E2E") != "1" {
		t.Skip("gated: set RYSH_SANDBOX_E2E=1 on macOS")
	}
}

// TestMacOSEscapeGitMetadataE2E: code left in .git for the user's next,
// unsandboxed, git command (gap01 E2) is refused; ordinary commits still work.
func TestMacOSEscapeGitMetadataE2E(t *testing.T) {
	gateMacE2E(t)
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	// The real git, not the /usr/bin/git xcrun shim: the shim writes a cache
	// in the real per-user temp dir, which this throwaway profile does not
	// allow, and its warning would make every refusal below ambiguous.
	gitBin := "/usr/bin/git"
	if out, err := exec.Command("/usr/bin/xcrun", "-f", "git").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		gitBin = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command(gitBin, "-C", l.ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	git := filepath.Join(l.ws, ".git")
	gitc := gitBin + " -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false "

	// Positive first and fatal, so a refusal below cannot pass vacuously.
	for name, sc := range map[string]string{
		"commit in workspace": "echo a > a.txt && " + gitc + "add a.txt && " + gitc + "commit -q -m a",
	} {
		if err := macRun(t, l, nil, sc); err != nil {
			t.Fatalf("%s: refused: %v", name, err)
		}
	}
	for name, sc := range map[string]string{
		"write a hook":             "printf '#!/bin/sh\\ntouch pwned\\n' > " + filepath.Join(git, "hooks", "post-commit"),
		"set core.hooksPath":       gitc + "config core.hooksPath " + l.ws,
		"set core.fsmonitor":       gitc + "config core.fsmonitor 'touch pwned'",
		"append to .git/config":    "echo '[core]' >> " + filepath.Join(git, "config"),
		"replace .git/config":      "mv " + filepath.Join(git, "config") + " " + filepath.Join(l.ws, "c.bak"),
		"rename .git away":         "mv " + git + " " + filepath.Join(l.ws, "dotgit"),
		"replace the hooks dir":    "mv " + filepath.Join(git, "hooks") + " " + filepath.Join(l.ws, "h.bak"),
		"create config.worktree":   "echo x > " + filepath.Join(git, "config.worktree"),
		"remove .git":              "rm -rf " + git,
		"symlink hooks elsewhere":  "rm -rf " + filepath.Join(git, "hooks") + " ; ln -s " + l.ws + " " + filepath.Join(git, "hooks"),
		"git init a second time":   gitBin + " init -q --template=" + l.ws + " .",
		"write via a hard link":    "ln " + filepath.Join(git, "config") + " " + filepath.Join(l.ws, "cfg") + " && echo x >> " + filepath.Join(l.ws, "cfg"),
		"hard link a secret":       "ln " + filepath.Join(l.home, ".ssh", "id_synthetic") + " " + filepath.Join(l.ws, "k"),
		"read a secret by symlink": "ln -sf " + filepath.Join(l.home, ".ssh", "id_synthetic") + " " + filepath.Join(l.ws, "s") + " && cat " + filepath.Join(l.ws, "s"),
	} {
		_ = macRun(t, l, nil, sc)
		// Judge by the host state, not the exit code: a script can fail for
		// another reason and still have done the damage.
		if err := gitMetaIntact(l.ws); err != nil {
			t.Errorf("%s: ESCAPED: %v", name, err)
			return
		}
	}
	if _, err := os.Stat(filepath.Join(l.ws, "k")); err == nil {
		t.Error("hard link to a secret was created in the workspace")
	}
	if data, err := os.ReadFile(filepath.Join(git, "config")); err != nil || strings.Contains(string(data), "hooksPath") || strings.Contains(string(data), "fsmonitor") {
		t.Errorf("config changed: %v %q", err, data)
	}
}

// gitMetaIntact checks the host side: .git is the same directory with its
// config, an empty hooks dir (git init's samples aside) and no config.worktree.
func gitMetaIntact(ws string) error {
	git := filepath.Join(ws, ".git")
	fi, err := os.Lstat(git)
	if err != nil || !fi.IsDir() {
		return errorf(".git is gone or not a directory (%v)", err)
	}
	if fi, err := os.Lstat(filepath.Join(git, "config")); err != nil || !fi.Mode().IsRegular() {
		return errorf(".git/config missing or replaced (%v)", err)
	}
	hfi, err := os.Lstat(filepath.Join(git, "hooks"))
	if err != nil || !hfi.IsDir() {
		return errorf(".git/hooks missing or replaced (%v)", err)
	}
	ents, _ := os.ReadDir(filepath.Join(git, "hooks"))
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".sample") {
			return errorf("hook %s was written", e.Name())
		}
	}
	if _, err := os.Lstat(filepath.Join(git, "config.worktree")); err == nil {
		return errorf("config.worktree was created")
	}
	return nil
}

func errorf(format string, a ...any) error { return fmt.Errorf(format, a...) }

// TestMacOSEscapeHostIPCE2E: with network ON (the default), the host's control
// plane is out of reach (gap01 E1): a loopback listener standing in for the
// session NATS and a unix socket standing in for a claude inbox / tmux /
// docker socket refuse the connection, while the agent's own provider port
// (ANTHROPIC_BASE_URL) stays reachable.
//
// Both listeners are this test's own; the sandboxed nc connects to nothing
// else. Run GREEN only: a RED of this case against the old profile is shown by
// the profile-text tests in escape_test.go (rr-msg-186).
func TestMacOSEscapeHostIPCE2E(t *testing.T) {
	gateMacE2E(t)
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	listen := func(network, addr string) net.Listener {
		ln, err := net.Listen(network, addr)
		if err != nil {
			t.Fatalf("listen %s %s: %v", network, addr, err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		return ln
	}
	control := listen("tcp", "127.0.0.1:0")
	proxy := listen("tcp", "127.0.0.1:0")
	// LEAD-CHANGES (4): IPv6 loopback, and the host's own LAN address, both
	// with the test's own listeners (nothing leaves the host).
	control6 := listen("tcp6", "[::1]:0")
	var lanCtl net.Listener
	lanIP := hostLANAddr()
	if lanIP != "" {
		lanCtl = listen("tcp4", lanIP+":0")
	} else {
		t.Log("no non-loopback IPv4 address on this host: LAN probe NOT RUN")
	}
	sockDir, err := os.MkdirTemp("/tmp", "g1s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "inbox.sock")
	listen("unix", sock)

	port := func(ln net.Listener) string { return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) }
	// The host itself can reach both (so a refusal below is the sandbox's).
	for _, ln := range []net.Listener{control6, lanCtl} {
		if ln == nil {
			continue
		}
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatalf("control: %s unreachable even unsandboxed: %v", ln.Addr(), err)
		}
		c.Close()
	}
	withProxy := func(s *Spec) {
		s.Env = append(s.Env, "ANTHROPIC_BASE_URL=http://127.0.0.1:"+port(proxy))
		s.ProxyPort, _ = strconv.Atoi(port(proxy)) // the session record's proxy port (R12)
	}
	// A one-line connect that says WHY it failed (nc -z is silent).
	nc := "/usr/bin/python3 -I -c 'import socket,sys; socket.create_connection((sys.argv[1], int(sys.argv[2])), 2).close()' "

	if out, err := exec.Command("/bin/sh", "-c", nc+"127.0.0.1 "+port(proxy)).CombinedOutput(); err != nil {
		t.Fatalf("control: the proxy listener is not reachable even unsandboxed: %v %s", err, out)
	}
	if err := macRun(t, l, withProxy, nc+"127.0.0.1 "+port(proxy)); err != nil {
		t.Fatalf("the provider port must stay reachable: %v", err)
	}
	// The unix-socket probe below is python: it must run here at all, or its
	// refusal would prove nothing.
	if err := macRun(t, l, withProxy, "/usr/bin/python3 -I -c 'import socket; socket.socket(socket.AF_UNIX).close()'"); err != nil {
		t.Fatalf("python does not run under the profile, so the unix-socket probe would be vacuous: %v", err)
	}
	for name, sc := range map[string]string{
		"loopback control port (127.0.0.1)": nc + "127.0.0.1 " + port(control),
		"loopback control port (localhost)": nc + "localhost " + port(control),
		"IPv6 loopback (::1)":               nc + "::1 " + port(control6),
		// nc -U -z reports failure even outside a sandbox; a one-line
		// connect is the honest probe.
		"unix socket of another process": "/usr/bin/python3 -I -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.settimeout(2); s.connect(sys.argv[1])' " + sock,
	} {
		if err := macRun(t, l, withProxy, sc); err == nil {
			t.Errorf("%s: REACHABLE from inside the sandbox", name)
		}
	}
	if lanCtl != nil {
		if err := macRun(t, l, withProxy, nc+lanIP+" "+port(lanCtl)); err == nil {
			t.Errorf("the host's own LAN address: REACHABLE from inside the sandbox")
		}
	}
}

// hostLANAddr is the first up, non-loopback IPv4 address of this host, or "".
func hostLANAddr() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				return ipn.IP.String()
			}
		}
	}
	return ""
}

// TestMacOSEscapeLaunchdAndAgentStartE2E: launchd will not run a job for the
// sandbox, and a real claude (when installed) still starts under the profile.
func TestMacOSEscapeLaunchdAndAgentStartE2E(t *testing.T) {
	gateMacE2E(t)
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(l.home, "launchd-was-here")
	label := "ai.rysh.gap01.e2e." + strconv.Itoa(os.Getpid())
	t.Cleanup(func() { _ = exec.Command("/bin/launchctl", "remove", label).Run() })
	if err := macRun(t, l, nil, "/bin/launchctl submit -l "+label+" -- /usr/bin/touch "+target); err == nil {
		t.Errorf("launchctl submit was accepted inside the sandbox")
	}
	if _, err := os.Stat(target); err == nil {
		t.Errorf("a launchd job ran outside the sandbox")
	}

	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Log("claude not installed: agent start check NOT RUN")
		return
	}
	// Network OFF: --version must not reach anything (an update check would
	// be egress), and deny network* is a superset of the network-on rules.
	p := builtinDefault()
	p.Backend = BackendMacOS
	p.Network = false
	s := l.spec(p)
	s.Argv = []string{"claude", "--version"}
	s.Env = []string{"PATH=" + filepath.Dir(claude) + ":/usr/bin:/bin", "ANTHROPIC_BASE_URL=http://127.0.0.1:1"}
	plan, err := (&MacOS{}).Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
	cmd.Env = plan.Env
	cmd.Dir = l.ws
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Claude Code") {
		t.Fatalf("claude --version under the sandbox: err=%v out=%s", err, out)
	}
	t.Logf("claude starts under the profile: %s", strings.TrimSpace(string(out)))
}

// TestMacOSEscapeNewRepoE2E: in a workspace with no .git, the agent cannot
// create one (and with it hooks and a config for the user's next git command).
func TestMacOSEscapeNewRepoE2E(t *testing.T) {
	gateMacE2E(t)
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	if err := macRun(t, l, nil, "echo ok > "+filepath.Join(l.ws, "w.txt")); err != nil {
		t.Fatalf("workspace not writable: %v", err)
	}
	for name, sc := range map[string]string{
		"mkdir .git":       "mkdir " + filepath.Join(l.ws, ".git"),
		"a .git file":      "echo 'gitdir: /tmp/x' > " + filepath.Join(l.ws, ".git"),
		"mkdir .git/hooks": "mkdir -p " + filepath.Join(l.ws, ".git", "hooks"),
	} {
		_ = macRun(t, l, nil, sc)
		if _, err := os.Lstat(filepath.Join(l.ws, ".git")); err == nil {
			t.Fatalf("%s: a .git was created in the workspace", name)
		}
	}
}

// TestMacOSEscapeSubmoduleAndRedirectionE2E (LEAD-CHANGES (2)): a submodule's
// git dir, its working-tree pointer file, a commondir redirect and module git
// dirs created during the run are all refused. Built offline with real git.
func TestMacOSEscapeSubmoduleAndRedirectionE2E(t *testing.T) {
	gateMacE2E(t)
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	gitBin := "/usr/bin/git"
	if out, err := exec.Command("/usr/bin/xcrun", "-f", "git").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		gitBin = strings.TrimSpace(string(out))
	}
	git := filepath.Join(l.ws, ".git")
	mod := filepath.Join(git, "modules", "sub")
	subWT := filepath.Join(l.ws, "sub")
	if out, err := exec.Command(gitBin, "-C", l.ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := os.MkdirAll(filepath.Dir(mod), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "--separate-git-dir", mod, subWT},
		{"--git-dir", mod, "config", "core.worktree", "../../../sub"},
	} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	pointer := filepath.Join(subWT, ".git")
	before := map[string][]byte{}
	for _, f := range []string{pointer, filepath.Join(mod, "config"), filepath.Join(git, "config")} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		before[f] = data
	}
	// Positive first: the submodule working tree itself is writable.
	if err := macRun(t, l, nil, "echo ok > "+filepath.Join(subWT, "w.txt")); err != nil {
		t.Fatalf("submodule working tree not writable: %v", err)
	}
	probes := map[string]string{
		"write a submodule hook":       "printf '#!/bin/sh\\n' > " + filepath.Join(mod, "hooks", "post-checkout"),
		"append to submodule config":   "echo '[core]' >> " + filepath.Join(mod, "config"),
		"rewrite the sub/.git ptr":     "echo 'gitdir: " + l.ws + "/evil' > " + pointer,
		"move the sub/.git ptr":        "mv " + pointer + " " + filepath.Join(subWT, "p.bak"),
		"rename the module git dir":    "mv " + mod + " " + filepath.Join(git, "modules", "moved"),
		"create .git/commondir":        "echo " + l.ws + "/evil > " + filepath.Join(git, "commondir"),
		"new module: hooks":            "mkdir -p " + filepath.Join(git, "modules", "new", "hooks") + " && echo x > " + filepath.Join(git, "modules", "new", "hooks", "post-checkout"),
		"new module: config":           "mkdir -p " + filepath.Join(git, "modules", "new2") + " && echo x > " + filepath.Join(git, "modules", "new2", "config"),
		"nested new module: commondir": "mkdir -p " + filepath.Join(mod, "modules", "x") + " && echo y > " + filepath.Join(mod, "modules", "x", "commondir"),
	}
	for name, sc := range probes {
		_ = macRun(t, l, nil, sc)
		for f, want := range before {
			if got, err := os.ReadFile(f); err != nil || string(got) != string(want) {
				t.Fatalf("%s: ESCAPED: %s changed (%v)", name, f, err)
			}
		}
		for _, f := range []string{
			filepath.Join(mod, "hooks", "post-checkout"),
			filepath.Join(git, "commondir"),
			filepath.Join(git, "modules", "new", "hooks", "post-checkout"),
			filepath.Join(git, "modules", "new2", "config"),
			filepath.Join(mod, "modules", "x", "commondir"),
		} {
			if _, err := os.Lstat(f); err == nil {
				t.Fatalf("%s: ESCAPED: %s exists", name, f)
			}
		}
	}
}

// TestMacOSEscapeNestedRepoAndCaseE2E (LEAD-CHANGES round 3, 2b + 2c): a NEW
// .git anywhere under the workspace (a dir, a pointer file, or a rename into
// place) is refused, so a gitlink plus a fresh newsub/.git cannot hand the
// user's next host-side `git status` a config the agent wrote. The metadata of
// a nested repository that already exists is held too. APFS is
// case-insensitive: CONFIG, HOOKS and .GIT are the same names, and are held
// the same way.
func TestMacOSEscapeNestedRepoAndCaseE2E(t *testing.T) {
	gateMacE2E(t)
	type fixture struct {
		l           layout
		git, vendor string
		before      map[string][]byte
	}
	setup := func(t *testing.T) fixture {
		l := newLayout(t)
		if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
			t.Fatal(err)
		}
		f := fixture{l: l, git: filepath.Join(l.ws, ".git"), vendor: filepath.Join(l.ws, "vendor", "lib", ".git"), before: map[string][]byte{}}
		for _, d := range []string{f.git, f.vendor} {
			writeFile(t, filepath.Join(d, "HEAD"), "ref: refs/heads/main\n")
			writeFile(t, filepath.Join(d, "config"), "[core]\n")
			f.before[filepath.Join(d, "config")] = []byte("[core]\n")
		}
		if err := os.MkdirAll(filepath.Join(f.vendor, "hooks"), 0o755); err != nil {
			t.Fatal(err)
		}
		return f
	}
	// Positive first: ordinary files, in new dirs and in the nested repo's
	// working tree, are writable.
	pos := setup(t)
	if err := macRun(t, pos.l, nil, "mkdir -p "+filepath.Join(pos.l.ws, "pos")+" && echo ok > "+filepath.Join(pos.l.ws, "pos", "a.txt")+
		" && echo ok > "+filepath.Join(pos.l.ws, "vendor", "lib", "b.txt")); err != nil {
		t.Fatalf("workspace not writable: %v", err)
	}
	probes := map[string]func(f fixture) string{
		"new nested .git dir": func(f fixture) string {
			return "mkdir -p " + filepath.Join(f.l.ws, "n1") + " && mkdir " + filepath.Join(f.l.ws, "n1", ".git")
		},
		"new nested .git file": func(f fixture) string {
			return "mkdir -p " + filepath.Join(f.l.ws, "n2") + " && echo 'gitdir: " + f.l.ws + "/evil' > " + filepath.Join(f.l.ws, "n2", ".git")
		},
		"rename a dir into .git": func(f fixture) string {
			return "mkdir -p " + filepath.Join(f.l.ws, "n3", "x") + " && mv " + filepath.Join(f.l.ws, "n3", "x") + " " + filepath.Join(f.l.ws, "n3", ".git")
		},
		"new nested .GIT": func(f fixture) string {
			return "mkdir -p " + filepath.Join(f.l.ws, "n4") + " && mkdir " + filepath.Join(f.l.ws, "n4", ".GIT")
		},
		"nested repo: append config": func(f fixture) string { return "echo '[core]' >> " + filepath.Join(f.vendor, "config") },
		"nested repo: write a hook":  func(f fixture) string { return "echo x > " + filepath.Join(f.vendor, "hooks", "post-checkout") },
		"nested repo: move .git away": func(f fixture) string {
			return "mv " + f.vendor + " " + filepath.Join(f.l.ws, "vendor", "lib", "g.bak")
		},
		"CONFIG of the repo": func(f fixture) string { return "echo '[core]' >> " + filepath.Join(f.git, "CONFIG") },
		"HOOKS of the repo": func(f fixture) string {
			return "mkdir -p " + filepath.Join(f.git, "HOOKS") + " && echo x > " + filepath.Join(f.git, "HOOKS", "pre-commit")
		},
		"new module CONFIG": func(f fixture) string {
			return "mkdir -p " + filepath.Join(f.git, "modules", "m1") + " && echo x > " + filepath.Join(f.git, "modules", "m1", "CONFIG")
		},
		"new module HOOKS": func(f fixture) string {
			return "mkdir -p " + filepath.Join(f.git, "modules", "m2", "HOOKS") + " && echo x > " + filepath.Join(f.git, "modules", "m2", "HOOKS", "post-checkout")
		},
		"config via upper-case path": func(f fixture) string {
			return "echo '[core]' >> " + filepath.Join(f.l.root, "HOME", strings.TrimPrefix(f.l.ws, f.l.home+"/"), ".git", "config")
		},
	}
	for name, probe := range probes {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			_ = macRun(t, f.l, nil, probe(f))
			for p, want := range f.before {
				if got, err := os.ReadFile(p); err != nil || string(got) != string(want) {
					t.Errorf("ESCAPED: %s changed (%v)", p, err)
				}
			}
			for _, p := range []string{
				filepath.Join(f.l.ws, "n1", ".git"), filepath.Join(f.l.ws, "n2", ".git"),
				filepath.Join(f.l.ws, "n3", ".git"), filepath.Join(f.l.ws, "n4", ".git"),
				filepath.Join(f.vendor, "hooks", "post-checkout"), filepath.Join(f.git, "hooks", "pre-commit"),
				filepath.Join(f.git, "modules", "m1", "config"), filepath.Join(f.git, "modules", "m2", "hooks", "post-checkout"),
			} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("ESCAPED: %s exists", p)
				}
			}
		})
	}
}

// TestMacOSMemoryEntriesHiddenE2E (R12, for gap12): agent memory lives in
// <rysh dir>/memory/entries. The sandbox hides the whole rysh dir, so an entry
// can be neither read nor written from inside; an agent proposes memory
// through a command, never by writing the file.
func TestMacOSMemoryEntriesHiddenE2E(t *testing.T) {
	gateMacE2E(t)
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	entries := filepath.Join(l.rysh, "memory", "entries")
	entry := filepath.Join(entries, "e1.md")
	writeFile(t, entry, "synthetic memory\n")
	// Positive control: the same read works outside the sandbox.
	if out, err := exec.Command("/bin/cat", entry).Output(); err != nil || string(out) != "synthetic memory\n" {
		t.Fatalf("control: entry not readable outside the sandbox: %v", err)
	}
	if err := macRun(t, l, nil, "echo ok > "+filepath.Join(l.ws, "w.txt")); err != nil {
		t.Fatalf("workspace not writable: %v", err)
	}
	for name, sc := range map[string]string{
		"read an entry":     "cat " + entry,
		"list the entries":  "ls " + entries,
		"append to entry":   "echo injected >> " + entry,
		"write a new entry": "echo injected > " + filepath.Join(entries, "e2.md"),
	} {
		if err := macRun(t, l, nil, sc); err == nil {
			t.Errorf("%s: ALLOWED inside the sandbox", name)
		}
	}
	if got, err := os.ReadFile(entry); err != nil || string(got) != "synthetic memory\n" {
		t.Errorf("ESCAPED: %s changed (%v)", entry, err)
	}
	if _, err := os.Lstat(filepath.Join(entries, "e2.md")); err == nil {
		t.Error("ESCAPED: a new memory entry was written")
	}
}

// Round 4: a module git dir is pinned by its own path, but the directories
// above it (.git/modules, a "/"-named module's parent, a module's own
// modules/) were not. Renaming one of them moves every module below it to a
// path no rule names: the agent writes hooks or config there and renames it
// back, inside one run.
func TestMacOSEscapeModulesParentRenameE2E(t *testing.T) {
	gateMacE2E(t)
	type fixture struct {
		l      layout
		git    string
		before map[string][]byte
	}
	setup := func(t *testing.T) fixture {
		l := newLayout(t)
		if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
			t.Fatal(err)
		}
		git, sub, inner := submoduleLayout(t, l.ws)
		slash := filepath.Join(git, "modules", "a", "b")
		writeFile(t, filepath.Join(slash, "HEAD"), "ref: refs/heads/main\n")
		writeFile(t, filepath.Join(slash, "config"), "[core]\n")
		f := fixture{l: l, git: git, before: map[string][]byte{}}
		for _, d := range []string{sub, inner, slash} {
			c, err := os.ReadFile(filepath.Join(d, "config"))
			if err != nil {
				t.Fatal(err)
			}
			f.before[filepath.Join(d, "config")] = c
		}
		return f
	}
	// away renames dir to tmp, writes the module below it, renames it back.
	away := func(dir, tmp, rel string) string {
		return "mv " + dir + " " + tmp +
			" ; mkdir -p " + filepath.Join(tmp, rel, "hooks") +
			" ; echo x > " + filepath.Join(tmp, rel, "hooks", "post-checkout") +
			" ; echo '[core]' >> " + filepath.Join(tmp, rel, "config") +
			" ; mv " + tmp + " " + dir
	}
	probes := map[string]func(f fixture) string{
		"rename .git/modules": func(f fixture) string {
			return away(filepath.Join(f.git, "modules"), filepath.Join(f.git, "mods"), "sub")
		},
		"rename a slash-named module's parent": func(f fixture) string {
			return away(filepath.Join(f.git, "modules", "a"), filepath.Join(f.git, "modules", "z"), "b")
		},
		"rename a module's own modules/": func(f fixture) string {
			return away(filepath.Join(f.git, "modules", "sub", "modules"), filepath.Join(f.git, "modules", "sub", "mm"), "inner")
		},
		"move a slash-named module's parent out of modules/": func(f fixture) string {
			return away(filepath.Join(f.git, "modules", "a"), filepath.Join(f.git, "xa"), "b")
		},
		"move a module's own modules/ out of modules/": func(f fixture) string {
			return away(filepath.Join(f.git, "modules", "sub", "modules"), filepath.Join(f.git, "xm"), "inner")
		},
	}
	for name, probe := range probes {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			_ = macRun(t, f.l, nil, probe(f))
			for p, want := range f.before {
				if got, err := os.ReadFile(p); err != nil || string(got) != string(want) {
					t.Errorf("ESCAPED: %s changed (%v)", p, err)
				}
				if _, err := os.Lstat(filepath.Join(filepath.Dir(p), "hooks", "post-checkout")); err == nil {
					t.Errorf("ESCAPED: hook written in %s", filepath.Dir(p))
				}
			}
			for _, p := range []string{filepath.Join(f.git, "mods"), filepath.Join(f.git, "modules", "z"), filepath.Join(f.git, "modules", "sub", "mm"), filepath.Join(f.git, "xa"), filepath.Join(f.git, "xm")} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("ESCAPED: %s exists (a pinned module was moved)", p)
				}
			}
		})
	}
}

// gap01 slice 3: the session-bus token file under the host home cannot be
// read or replaced from inside a macOS sandbox; the same read works outside.
func TestMacOSBusTokenHiddenE2E(t *testing.T) {
	gateMacE2E(t)
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(l.home, ".rysh-run", "nats-4222.token")
	writeFile(t, tok, "synthetic-token\n")
	if out, err := exec.Command("/bin/cat", tok).Output(); err != nil || string(out) != "synthetic-token\n" {
		t.Fatalf("control: not readable outside the sandbox: %v", err)
	}
	if err := macRun(t, l, nil, "cat "+tok); err == nil {
		t.Error("ESCAPED: the token file was readable inside the sandbox")
	}
	if err := macRun(t, l, nil, "ls "+filepath.Dir(tok)); err == nil {
		t.Error("ESCAPED: the token dir was listable inside the sandbox")
	}
	_ = macRun(t, l, nil, "echo x > "+tok)
	if got, _ := os.ReadFile(tok); string(got) != "synthetic-token\n" {
		t.Error("ESCAPED: the token file was rewritten from inside the sandbox")
	}
}
