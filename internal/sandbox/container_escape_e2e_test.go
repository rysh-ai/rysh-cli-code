// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// TestContainerEscapeGitMetadataE2E: gap01 E2 under the REAL container backend.
// Same gate, local image (--pull never) and throwaway tree under $HOME as
// TestContainerRealDockerE2E; network off. The probes are plain shell, so the
// image needs no git.
func TestContainerEscapeGitMetadataE2E(t *testing.T) {
	image := os.Getenv("RYSH_SANDBOX_E2E_IMAGE")
	if os.Getenv("RYSH_SANDBOX_DOCKER_E2E") != "1" || image == "" {
		t.Skip("gated: set RYSH_SANDBOX_DOCKER_E2E=1 and RYSH_SANDBOX_E2E_IMAGE=<local image with /bin/sh>")
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(realHome, ".rysh-sandbox-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, _ = filepath.EvalSymlinks(base)
	home := filepath.Join(base, "home")
	ws := filepath.Join(home, "proj")
	rysh := filepath.Join(ws, ".rysh")
	sbxHome := HomeDir(rysh, "box")
	writeFile(t, filepath.Join(rysh, "secrets", "x"), "synthetic")
	if out, err := exec.Command("git", "-C", ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	// A submodule, built offline (LEAD-CHANGES (2)).
	mod := filepath.Join(ws, ".git", "modules", "sub")
	subWT := filepath.Join(ws, "sub")
	if err := os.MkdirAll(filepath.Dir(mod), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "--separate-git-dir", mod, subWT},
		{"--git-dir", mod, "config", "core.worktree", "../../../sub"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	pointer := filepath.Join(subWT, ".git")
	ptrBefore, _ := os.ReadFile(pointer)
	modCfgBefore, _ := os.ReadFile(filepath.Join(mod, "config"))
	if err := PrepareHome(sbxHome, filepath.Join(home, ".claude"), nil); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(ws, ".git")
	n := 0
	run := func(script string) error {
		n++
		s := Spec{
			Profile:   Profile{Name: "box", Backend: BackendContainer, Image: image, Network: false},
			Workspace: ws, Home: sbxHome, HostHome: home, RyshDir: rysh,
			Argv: []string{"/bin/sh", "-c", script},
			Env:  []string{"PATH=" + os.Getenv("PATH"), "LANG=C"},
			ID:   e2eID(t, "g1esc"+strings.Repeat("x", n%3)+string(rune('a'+n%26))),
		}
		plan, err := (&Container{UID: os.Getuid(), GID: os.Getgid()}).Prepare(s)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
		cmd.Env = plan.Env
		out, err := cmd.CombinedOutput()
		t.Logf("%q -> err=%v out=%s", script, err, strings.TrimSpace(string(out)))
		return err
	}
	if err := run("echo a > " + filepath.Join(ws, "a.txt") + " && echo obj > " + filepath.Join(git, "objects", "probe")); err != nil {
		t.Fatalf("workspace / .git objects not writable (container did not run?): %v", err)
	}
	for name, sc := range map[string]string{
		"write a hook":            "printf '#!/bin/sh\\n' > " + filepath.Join(git, "hooks", "post-commit"),
		"append to .git/config":   "echo '[core]' >> " + filepath.Join(git, "config"),
		"replace .git/config":     "mv " + filepath.Join(git, "config") + " " + filepath.Join(ws, "c.bak"),
		"rename .git away":        "mv " + git + " " + filepath.Join(ws, "dotgit"),
		"replace the hooks dir":   "mv " + filepath.Join(git, "hooks") + " " + filepath.Join(ws, "h.bak"),
		"write config.worktree":   "echo x > " + filepath.Join(git, "config.worktree"),
		"remove .git":             "rm -rf " + git,
		"write a submodule hook":  "printf '#!/bin/sh\\n' > " + filepath.Join(mod, "hooks", "post-checkout"),
		"append submodule config": "echo '[core]' >> " + filepath.Join(mod, "config"),
		"rewrite sub/.git ptr":    "echo 'gitdir: " + ws + "/evil' > " + pointer,
		"move sub/.git ptr":       "mv " + pointer + " " + filepath.Join(subWT, "p.bak"),
		"rename module git dir":   "mv " + mod + " " + filepath.Join(ws, ".git", "modules", "moved"),
	} {
		_ = run(sc)
		if err := gitMetaIntactAllowEmptyWorktreeConfig(ws); err != nil {
			t.Errorf("%s: ESCAPED: %v", name, err)
			return
		}
		if got, err := os.ReadFile(pointer); err != nil || string(got) != string(ptrBefore) {
			t.Errorf("%s: ESCAPED: sub/.git changed (%v)", name, err)
			return
		}
		if got, err := os.ReadFile(filepath.Join(mod, "config")); err != nil || string(got) != string(modCfgBefore) {
			t.Errorf("%s: ESCAPED: submodule config changed (%v)", name, err)
			return
		}
		if _, err := os.Lstat(filepath.Join(mod, "hooks", "post-checkout")); err == nil {
			t.Errorf("%s: ESCAPED: submodule hook written", name)
			return
		}
	}
}

// gitMetaIntactAllowEmptyWorktreeConfig is gitMetaIntact for the container
// backend, which prepares an EMPTY config.worktree to hold it read-only.
func gitMetaIntactAllowEmptyWorktreeConfig(ws string) error {
	cw := filepath.Join(ws, ".git", "config.worktree")
	if fi, err := os.Stat(cw); err == nil {
		if fi.Size() != 0 {
			return errorf("config.worktree was written")
		}
		tmp := cw + ".held"
		if err := os.Rename(cw, tmp); err != nil {
			return err
		}
		defer os.Rename(tmp, cw)
	}
	return gitMetaIntact(ws)
}

// Round 4 under the REAL container backend: the directories above a module
// git dir (.git/modules, a "/"-named module's parent, a module's own
// modules/) are bound onto themselves, so they cannot be renamed. Before, run
// 1 renamed one away, run 2's walk found nothing there to hold, and run 2
// renamed it back and wrote the module's hooks and config.
func TestContainerEscapeModulesParentRenameE2E(t *testing.T) {
	image := os.Getenv("RYSH_SANDBOX_E2E_IMAGE")
	if os.Getenv("RYSH_SANDBOX_DOCKER_E2E") != "1" || image == "" {
		t.Skip("gated: set RYSH_SANDBOX_DOCKER_E2E=1 and RYSH_SANDBOX_E2E_IMAGE=<local image with /bin/sh>")
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(realHome, ".rysh-sandbox-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, _ = filepath.EvalSymlinks(base)
	home := filepath.Join(base, "home")
	ws := filepath.Join(home, "proj")
	rysh := filepath.Join(ws, ".rysh")
	sbxHome := HomeDir(rysh, "box")
	git, sub, inner := submoduleLayout(t, ws)
	slash := filepath.Join(git, "modules", "a", "b")
	writeFile(t, filepath.Join(slash, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(slash, "config"), "[core]\n")
	before := map[string][]byte{}
	for _, d := range []string{sub, inner, slash} {
		c, err := os.ReadFile(filepath.Join(d, "config"))
		if err != nil {
			t.Fatal(err)
		}
		before[d] = c
	}
	if err := PrepareHome(sbxHome, filepath.Join(home, ".claude"), nil); err != nil {
		t.Fatal(err)
	}
	n := 0
	run := func(script string) {
		n++
		s := Spec{
			Profile:   Profile{Name: "box", Backend: BackendContainer, Image: image, Network: false},
			Workspace: ws, Home: sbxHome, HostHome: home, RyshDir: rysh,
			Argv: []string{"/bin/sh", "-c", script},
			Env:  []string{"PATH=" + os.Getenv("PATH"), "LANG=C"},
			ID:   e2eID(t, "g1ren"+string(rune('a'+n%26))),
		}
		plan, err := (&Container{UID: os.Getuid(), GID: os.Getgid()}).Prepare(s)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
		cmd.Env = plan.Env
		out, err := cmd.CombinedOutput()
		t.Logf("%q -> err=%v out=%s", script, err, strings.TrimSpace(string(out)))
	}
	moves := [][2]string{
		{filepath.Join(git, "modules", "sub", "modules"), filepath.Join(git, "xm")},
		{filepath.Join(git, "modules", "a"), filepath.Join(git, "xa")},
		{filepath.Join(git, "modules"), filepath.Join(git, "mods")},
	}
	var away, back []string
	for _, m := range moves {
		away = append(away, "mv "+m[0]+" "+m[1])
		back = append([]string{"mv " + m[1] + " " + m[0]}, back...)
	}
	run(strings.Join(away, " ; "))
	var write []string
	for _, d := range []string{sub, inner, slash} {
		write = append(write, "mkdir -p "+filepath.Join(d, "hooks"), "echo x > "+filepath.Join(d, "hooks", "post-checkout"), "echo '[core]' >> "+filepath.Join(d, "config"))
	}
	run(strings.Join(append(back, write...), " ; "))
	for d, want := range before {
		if got, err := os.ReadFile(filepath.Join(d, "config")); err != nil || string(got) != string(want) {
			t.Errorf("ESCAPED: %s/config changed (%v)", d, err)
		}
		if _, err := os.Lstat(filepath.Join(d, "hooks", "post-checkout")); err == nil {
			t.Errorf("ESCAPED: hook written in %s", d)
		}
	}
	for _, m := range moves {
		if _, err := os.Lstat(m[1]); err == nil {
			t.Errorf("ESCAPED: %s was renamed to %s", m[0], m[1])
		}
	}
}

// gap01 slice 3 under the REAL container backend, network: on: the documented
// limit was that such a container can reach the host's session NATS. With the
// bus token it can still open a TCP connection on some setups, but a tokenless
// CONNECT is refused: no PONG ever comes back. The server here is a real NATS
// with a token, listening on every host address so each route is tried.
func TestContainerCannotDriveTheTokenBusE2E(t *testing.T) {
	image := os.Getenv("RYSH_SANDBOX_E2E_IMAGE")
	if os.Getenv("RYSH_SANDBOX_DOCKER_E2E") != "1" || image == "" {
		t.Skip("gated: set RYSH_SANDBOX_DOCKER_E2E=1 and RYSH_SANDBOX_E2E_IMAGE=<local image with /bin/sh and nc>")
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(realHome, ".rysh-sandbox-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, _ = filepath.EvalSymlinks(base)
	home := filepath.Join(base, "home")
	ws := filepath.Join(home, "proj")
	rysh := filepath.Join(ws, ".rysh")
	sbxHome := HomeDir(rysh, "box")
	writeFile(t, filepath.Join(ws, ".git", "HEAD"), "ref: refs/heads/main\n")
	if err := PrepareHome(sbxHome, filepath.Join(home, ".claude"), nil); err != nil {
		t.Fatal(err)
	}
	tok := strings.Repeat("ab", 32)
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "0.0.0.0", Port: natsserver.RANDOM_PORT, Authorization: tok, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	defer ns.Shutdown()
	port := strconv.Itoa(ns.Addr().(*net.TCPAddr).Port)
	// Positive control on the host: the server answers, and refuses a tokenless CONNECT.
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte("CONNECT {\"verbose\":false}\r\nPING\r\n"))
	ctl, _ := io.ReadAll(c)
	c.Close()
	if !strings.Contains(string(ctl), "Authorization Violation") {
		t.Fatalf("control: the host server did not refuse a tokenless client: %q", ctl)
	}
	script := `for h in host.docker.internal host.lima.internal 192.168.5.2 172.17.0.1 $(ip route 2>/dev/null | awk '/default/{print $3}'); do
  printf 'CONNECT {"verbose":false}\r\nPING\r\n' | nc -w 2 $h ` + port + ` 2>/dev/null | tr -d '\r' | sed "s|^|$h: |"
done; echo done`
	s := Spec{
		Profile:   Profile{Name: "box", Backend: BackendContainer, Image: image, Network: true},
		Workspace: ws, Home: sbxHome, HostHome: home, RyshDir: rysh,
		Argv: []string{"/bin/sh", "-c", script},
		Env:  []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C"},
		ID:   e2eID(t, "g1bustok"),
	}
	plan, err := (&Container{UID: os.Getuid(), GID: os.Getgid()}).Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
	cmd.Env = plan.Env
	out, _ := cmd.CombinedOutput()
	t.Logf("container probe:\n%s", out)
	if !strings.Contains(string(out), "done") {
		t.Fatalf("the container did not run the probe: %s", out)
	}
	if strings.Contains(string(out), "PONG") {
		t.Error("ESCAPED: a tokenless client in a network: on container was answered PONG by the bus")
	}
	if strings.Contains(string(out), "Authorization Violation") {
		t.Log("reached the host bus and was refused: the documented container limit is closed by the token")
	} else {
		t.Log("no route from the container to the host server on this setup")
	}
}
