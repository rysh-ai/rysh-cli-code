// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestContainerRealDockerE2E runs the REAL container backend. ENV-GATED: only
// with RYSH_SANDBOX_DOCKER_E2E=1 and RYSH_SANDBOX_E2E_IMAGE naming an image
// that is ALREADY LOCAL (it is run with --pull never; nothing is fetched) and
// has /bin/sh. The profile is network off (--network none), so the container
// has no network at all. The throwaway tree lives under the real $HOME because
// a VM-backed docker (colima, Docker Desktop) only shares $HOME; it is removed
// afterwards.
func TestContainerRealDockerE2E(t *testing.T) {
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
	writeFile(t, filepath.Join(home, ".ssh", "id_synthetic"), "not a key")
	writeFile(t, filepath.Join(home, "other", "notes.txt"), "outside")
	writeFile(t, filepath.Join(rysh, "secrets", "x"), "synthetic")
	writeFile(t, filepath.Join(ws, "in.txt"), "inside")
	if err := PrepareHome(sbxHome, filepath.Join(home, ".claude"), nil); err != nil {
		t.Fatal(err)
	}

	run := func(script string) (string, error) {
		s := Spec{
			Profile:   Profile{Name: "box", Backend: BackendContainer, Image: image, Network: false},
			Workspace: ws, Home: sbxHome, HostHome: home, RyshDir: rysh,
			Argv: []string{"/bin/sh", "-c", script},
			Env:  []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "SYNTHETIC_VISIBLE=1"},
			ID:   "e2e" + strings.ToLower(filepath.Base(base)[len(".rysh-sandbox-e2e-"):]),
		}
		s.ID = strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return -1
		}, s.ID)
		s.ID = e2eID(t, s.ID)
		plan, err := (&Container{UID: os.Getuid(), GID: os.Getgid()}).Prepare(s)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
		cmd.Env = plan.Env
		out, err := cmd.CombinedOutput()
		t.Logf("%q -> err=%v out=%s", script, err, strings.TrimSpace(string(out)))
		return string(out), err
	}

	// The positive case FIRST and FATAL: every refusal below would otherwise
	// "pass" vacuously when the container never started at all.
	if out, err := run("cat " + filepath.Join(ws, "in.txt") + " && echo w > " + filepath.Join(ws, "w.txt") + " && echo h > \"$HOME/h.txt\""); err != nil || !strings.Contains(out, "inside") {
		t.Fatalf("workspace/home not usable (container did not run?): %v", err)
	}
	for name, sc := range map[string]string{
		"~/.ssh not mounted":     "cat " + filepath.Join(home, ".ssh", "id_synthetic"),
		"other home not mounted": "cat " + filepath.Join(home, "other", "notes.txt"),
		"rysh secrets hidden":    "cat " + filepath.Join(rysh, "secrets", "x"),
	} {
		if _, err := run(sc); err == nil {
			t.Errorf("%s: ALLOWED inside the container", name)
		}
	}
	// network off, checked WITHOUT any connect attempt (standing rule: no
	// egress probes): --network none leaves only the loopback interface.
	if out, err := run("ls /sys/class/net"); err != nil || strings.TrimSpace(out) != "lo" {
		t.Errorf("network off: interfaces = %q (err %v), want only lo", strings.TrimSpace(out), err)
	}
	out, err := run("env | cut -d= -f1 | sort")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HOME", "LANG", "RYSH_SANDBOX", "SYNTHETIC_VISIBLE"} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("env name %s missing inside", want)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "w.txt")); err != nil {
		t.Errorf("write in workspace did not land on the host: %v", err)
	}
}

// TestContainerRealDockerPolicyRulesE2E: the policy fs rules (design 068) under
// the REAL container backend. Same gate, image rules and throwaway tree under
// $HOME as above; network off.
func TestContainerRealDockerPolicyRulesE2E(t *testing.T) {
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
	src := filepath.Join(ws, "src")
	gen := filepath.Join(src, "gen")
	env := filepath.Join(ws, ".env")
	sbxHome := HomeDir(rysh, "box")
	writeFile(t, filepath.Join(gen, "keep"), "")
	writeFile(t, env, "SYNTHETIC=1")
	writeFile(t, filepath.Join(ws, "readme.txt"), "inside")
	if err := PrepareHome(sbxHome, filepath.Join(home, ".claude"), nil); err != nil {
		t.Fatal(err)
	}
	id := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, "e2epol"+strings.ToLower(filepath.Base(base)))

	run := func(script string) (string, error) {
		s := Spec{
			Profile:   Profile{Name: "box", Backend: BackendContainer, Image: image, Network: false},
			Workspace: ws, Home: sbxHome, HostHome: home, RyshDir: rysh,
			Argv:  []string{"/bin/sh", "-c", script},
			Env:   []string{"PATH=" + os.Getenv("PATH"), "LANG=C"},
			Rules: PathRules{ReadOnly: []string{ws, gen}, ReadWrite: []string{src}, Deny: []string{env}},
			ID:    e2eID(t, id[:min(len(id), 30)]),
		}
		plan, err := (&Container{UID: os.Getuid(), GID: os.Getgid()}).Prepare(s)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
		cmd.Env = plan.Env
		out, err := cmd.CombinedOutput()
		t.Logf("%q -> err=%v out=%s", script, err, strings.TrimSpace(string(out)))
		return string(out), err
	}
	// Positive cases first and fatal.
	if out, err := run("cat " + filepath.Join(ws, "readme.txt") + " && echo ok > " + filepath.Join(src, "w.txt") + " && echo h > \"$HOME/h.txt\""); err != nil || !strings.Contains(out, "inside") {
		t.Fatalf("read-only workspace / src carve-out / home not usable: %v", err)
	}
	for name, sc := range map[string]string{
		"write in read-only workspace": "echo x > " + filepath.Join(ws, "w.txt"),
		"write in gen (ro inside rw)":  "echo x > " + filepath.Join(gen, "w.txt"),
		"read denied .env":             "grep -q SYNTHETIC " + env,
	} {
		if _, err := run(sc); err == nil {
			t.Errorf("%s: ALLOWED inside the container", name)
		}
	}
}
