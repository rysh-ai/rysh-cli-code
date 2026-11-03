// SPDX-License-Identifier: Apache-2.0

package agentenv

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAccountToken is a fixture, not credential-shaped.
const fakeAccountToken = "fixture-account-b"

func homeWithAccount(t *testing.T, mode os.FileMode) string {
	t.Helper()
	home := t.TempDir()
	dir := ClaudeAccountDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := ClaudeAccountTokenFile(dir)
	if err := os.WriteFile(p, []byte(fakeAccountToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return home
}

func runMain(t *testing.T, kind, home string, extra ...string) (env []string, argv []string, stderr string) {
	t.Helper()
	fake := func(_ string, a, e []string) error { argv, env = a, e; return nil }
	in := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CODE_OAUTH_TOKEN=FAKE-OLD", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=9", "RYSH_PANE=p"}
	var errb bytes.Buffer
	args := append([]string{"--agent", kind}, extra...)
	args = append(args, "--", "/bin/echo", "hi")
	if rc := Main(args, in, &bytes.Buffer{}, &errb, fake); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, errb.String())
	}
	return env, argv, errb.String()
}

func countName(env []string, name string) (string, int) {
	v, n := "", 0
	for _, kv := range env {
		if k, val, _ := strings.Cut(kv, "="); k == name {
			v, n = val, n+1
		}
	}
	return v, n
}

// An active account replaces the token claude inherits, exactly once, drops
// any inherited descriptor variable, and the value appears nowhere but the
// exec'd environment.
func TestExecCleanUsesTheActiveClaudeAccount(t *testing.T) {
	home := homeWithAccount(t, 0o600)
	env, argv, stderr := runMain(t, "claude", home, "--explain")
	if v, n := countName(env, ClaudeOAuthTokenEnv); v != fakeAccountToken || n != 1 {
		t.Fatalf("token not set from the account file (n=%d)", n)
	}
	if _, n := countName(env, ClaudeOAuthTokenFDEnv); n != 0 {
		t.Fatal("an inherited descriptor variable survived")
	}
	if strings.Contains(strings.Join(argv, " "), fakeAccountToken) || strings.Contains(stderr, fakeAccountToken) {
		t.Fatal("the token leaked into argv or stderr")
	}
	if !strings.Contains(stderr, ClaudeOAuthTokenEnv) {
		t.Fatalf("--explain should name the variable it set: %q", stderr)
	}
}

// No file: nothing changes, so claude keeps its own login.
func TestExecCleanWithoutAccountLeavesEnvAlone(t *testing.T) {
	env, _, stderr := runMain(t, "claude", t.TempDir())
	if v, n := countName(env, ClaudeOAuthTokenEnv); v != "FAKE-OLD" || n != 1 {
		t.Fatalf("env changed without an account (n=%d)", n)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
}

// A token file others can read is refused, with a line that says why.
func TestExecCleanRefusesAWorldReadableAccount(t *testing.T) {
	home := homeWithAccount(t, 0o644)
	env, _, stderr := runMain(t, "claude", home)
	if v, _ := countName(env, ClaudeOAuthTokenEnv); v != "FAKE-OLD" {
		t.Fatal("a world-readable token file was used")
	}
	if !strings.Contains(stderr, "readable by others") || strings.Contains(stderr, fakeAccountToken) {
		t.Fatalf("stderr = %q", stderr)
	}
}

// A symlinked token file is refused (e20 Q1 hardening).
func TestExecCleanRefusesASymlinkedAccount(t *testing.T) {
	home := t.TempDir()
	dir := ClaudeAccountDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte(fakeAccountToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ClaudeAccountTokenFile(dir)); err != nil {
		t.Fatal(err)
	}
	env, _, stderr := runMain(t, "claude", home)
	if v, _ := countName(env, ClaudeOAuthTokenEnv); v != "FAKE-OLD" {
		t.Fatal("a symlinked token file was used")
	}
	if !strings.Contains(stderr, "not a regular file") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// codex never gets a claude token.
func TestExecCleanAccountIsClaudeOnly(t *testing.T) {
	home := homeWithAccount(t, 0o600)
	env, _, _ := runMain(t, "codex", home)
	for _, kv := range env {
		if strings.Contains(kv, fakeAccountToken) {
			t.Fatal("codex received the claude account")
		}
	}
}

func TestClaudeAccountDirNeedsHome(t *testing.T) {
	if ClaudeAccountDir("") != "" {
		t.Fatal("empty HOME must disable the account")
	}
	if got := ClaudeAccountDir("/h"); got != filepath.Join("/h", ".rysh", "claude-account") {
		t.Fatalf("dir = %q", got)
	}
}

// The real exec: the agent process sees the account token (verdicts only,
// never a value) and no descriptor variable.
func TestRealExecCarriesTheAccount(t *testing.T) {
	home := homeWithAccount(t, 0o600)
	probe := filepath.Join(t.TempDir(), "account-env-probe")
	script := "#!/bin/sh\n" +
		"[ \"$CLAUDE_CODE_OAUTH_TOKEN\" = " + fakeAccountToken + " ] && echo token-from-account || echo token-other\n" +
		"[ -n \"${CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR:+x}\" ] && echo fd-present || echo fd-absent\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "agentenv-exec-clean", "--agent", "claude", "--", probe)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CODE_OAUTH_TOKEN=FAKE-OLD"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("exec-clean: %v", err)
	}
	if got := strings.Join(strings.Fields(string(out)), " "); got != "token-from-account fd-absent" {
		t.Fatalf("probe said %q", got)
	}
}
