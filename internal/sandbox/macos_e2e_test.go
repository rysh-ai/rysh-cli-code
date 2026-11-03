// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMacOSRealSandboxExecE2E runs the REAL /usr/bin/sandbox-exec with a
// generated profile. ENV-GATED: it runs only with RYSH_SANDBOX_E2E=1 on macOS
// (named as such in the whole-suite counts as a skip otherwise). Throwaway dirs
// only; the "agent" is /bin/sh; the profile is network on, so no network-guard
// RED is involved.
func TestMacOSRealSandboxExecE2E(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("RYSH_SANDBOX_E2E") != "1" {
		t.Skip("gated: set RYSH_SANDBOX_E2E=1 on macOS")
	}
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(l.home, "other")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outside, "notes.txt"), "outside")

	run := func(script string) error {
		p := builtinDefault()
		p.Backend = BackendMacOS
		s := l.spec(p)
		s.Argv = []string{"/bin/sh", "-c", script}
		// Its own TMPDIR: the throwaway home itself lives under the real
		// $TMPDIR, which the profile lets the agent write.
		s.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + filepath.Join(l.ws, "tmp")}
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

	// The read probe must not depend on map order: the file exists before
	// any probe runs (a read that ran before the write failed with ENOENT).
	writeFile(t, filepath.Join(l.ws, "w.txt"), "ok\n")
	allowed := map[string]string{
		"write in workspace":    "echo ok > " + filepath.Join(l.ws, "w.txt"),
		"write in sandbox home": "echo ok > \"$HOME/h.txt\"",
		"read in workspace":     "cat " + filepath.Join(l.ws, "w.txt"),
	}
	denied := map[string]string{
		"read ~/.ssh":            "cat " + filepath.Join(l.home, ".ssh", "id_synthetic"),
		"read ~/.aws":            "cat " + filepath.Join(l.home, ".aws", "credentials"),
		"read ~/.rysh_env":       "cat " + filepath.Join(l.home, ".rysh_env"),
		"read host .claude cred": "cat " + filepath.Join(l.home, ".claude", ".credentials.json"),
		"read rysh secrets":      "cat " + filepath.Join(l.rysh, "secrets", "x"),
		"read other home file":   "cat " + filepath.Join(outside, "notes.txt"),
		"write outside":          "echo x > " + filepath.Join(outside, "w.txt"),
		"list home":              "ls " + l.home,
	}
	for name, sc := range allowed {
		if err := run(sc); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	for name, sc := range denied {
		if err := run(sc); err == nil {
			t.Errorf("%s: ALLOWED inside the sandbox", name)
		}
	}
}

// TestMacOSRealSandboxExecPolicyRulesE2E: the policy fs rules (design 068) under
// the REAL sandbox-exec. Same gate and throwaway layout as above: the workspace
// read-only with a src/ carve-out, gen/ read-only again inside it, and a denied
// .env.
func TestMacOSRealSandboxExecPolicyRulesE2E(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("RYSH_SANDBOX_E2E") != "1" {
		t.Skip("gated: set RYSH_SANDBOX_E2E=1 on macOS")
	}
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(l.ws, "src")
	gen := filepath.Join(src, "gen")
	env := filepath.Join(l.ws, ".env")
	if err := os.MkdirAll(gen, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, env, "SYNTHETIC=1")
	writeFile(t, filepath.Join(l.ws, "readme.txt"), "inside")
	tmp := filepath.Join(l.root, "agent-tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}

	run := func(script string) error {
		p := builtinDefault()
		p.Backend = BackendMacOS
		s := l.spec(p)
		s.Argv = []string{"/bin/sh", "-c", script}
		s.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + tmp}
		s.Rules = PathRules{ReadOnly: []string{l.ws, gen}, ReadWrite: []string{src}, Deny: []string{env}}
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
	// Positive cases first and fatal: a refusal below must not pass vacuously.
	for name, sc := range map[string]string{
		"read in read-only workspace": "cat " + filepath.Join(l.ws, "readme.txt"),
		"write in src carve-out":      "echo ok > " + filepath.Join(src, "w.txt"),
		"write in sandbox home":       "echo ok > \"$HOME/h.txt\"",
	} {
		if err := run(sc); err != nil {
			t.Fatalf("%s: refused: %v", name, err)
		}
	}
	for name, sc := range map[string]string{
		"write in read-only workspace": "echo x > " + filepath.Join(l.ws, "w.txt"),
		"write in gen (ro inside rw)":  "echo x > " + filepath.Join(gen, "w.txt"),
		"read denied .env":             "cat " + env,
		"write denied .env":            "echo x > " + env,
	} {
		if err := run(sc); err == nil {
			t.Errorf("%s: ALLOWED inside the sandbox", name)
		}
	}
}
