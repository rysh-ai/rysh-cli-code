// SPDX-License-Identifier: Apache-2.0

package actors

// The agent exec lines rysh TYPES into a pane run with credentials scrubbed
// (root-0472; ORDER-62 found ~/.bashrc exporting AWS keys into every pane, which
// only the exec of the agent itself can undo). Each real builder's line is run
// through `bash -c` with the agent binary swapped for a STUB by absolute path,
// NOT named claude/codex (rr-msg-181), that prints its env NAMES only. FAKE
// values only; no real credential is read or printed.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/agentenv"
)

// TestMain lets this test binary stand in for `rysh exec-clean`, so the typed
// lines are exercised end to end without building cmd/rysh.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "exec-clean" {
		os.Exit(agentenv.Main(os.Args[2:], os.Environ(), os.Stdout, os.Stderr, agentenv.SysExec))
	}
	os.Exit(m.Run())
}

var agentWord = regexp.MustCompile(`(^|[\s;&|])(claude|codex)(\s)`)

// runTyped runs line in bash with fake env, the agent replaced by the stub, and
// returns the NAMES the stub saw.
func runTyped(t *testing.T, line string, env []string) map[string]bool {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "agent-env-probe")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nenv | cut -d= -f1 | sort\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Swap the agent COMMAND word for the stub, but not exec-clean's own
	// `--agent <kind>` argument.
	line = strings.ReplaceAll(line, "--agent claude", "--agent \x00CLAUDE")
	line = strings.ReplaceAll(line, "--agent codex", "--agent \x00CODEX")
	line = agentWord.ReplaceAllString(line, "${1}"+stub+"${3}")
	line = strings.ReplaceAll(line, "\x00CLAUDE", "claude")
	line = strings.ReplaceAll(line, "\x00CODEX", "codex")
	cmd := exec.Command("/bin/bash", "-c", line)
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("typed line failed: %v\n%s", err, out)
	}
	got := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.Contains(l, "FAKE-") {
			t.Fatalf("a value reached the output: %q", l)
		}
		got[strings.TrimSpace(l)] = true
	}
	return got
}

func scrubFakeEnv(extra ...string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent-fake-home"}
	names := append(append([]string{}, agentenv.StaticNames()...),
		"FOO_TOKEN", "BAR_PASSWORD", "ZZ_API_KEY", "MY_PAT", "X_SECRET_Y", "AWS_FAKE_THING",
		"RYSH_PANE", "GIT_EDITOR", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_BASE_URL")
	for _, n := range append(names, extra...) {
		env = append(env, n+"=FAKE-"+n)
	}
	return env
}

func TestTypedAgentLinesScrubCredentials(t *testing.T) {
	agentenv.SetBinaryForTest(os.Args[0])
	claudeLines := map[string]string{
		"##claude launch (workspace_pane_claude.go)":   claudeLaunchCommand("sid-1", "", ""),
		"native claude resume (native_agent.go)":       nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: "sid-1", Resume: true}),
		"native claude launch (native_agent.go)":       nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: "sid-1"}),
		"native claude continue (native_agent.go)":     nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, Resume: true}),
		"kill/respawn claude (workspace_ansa_kill.go)": ansaResumeCommand(nativeAgentClaude, "sid-1", ""),
	}
	codexLines := map[string]string{
		"native codex resume (native_agent.go)":       nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentCodex, SessionID: "sid-2", Resume: true}),
		"native codex launch (native_agent.go)":       nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentCodex}),
		"kill/respawn codex (workspace_ansa_kill.go)": ansaResumeCommand(nativeAgentCodex, "sid-2", ""),
	}
	for site, line := range claudeLines {
		if strings.Contains(line, "FAKE-") {
			t.Fatalf("%s: a value in the typed line", site)
		}
		got := runTyped(t, line, scrubFakeEnv())
		for _, n := range append(agentenv.StaticNames(), "FOO_TOKEN", "BAR_PASSWORD", "ZZ_API_KEY", "MY_PAT", "X_SECRET_Y", "AWS_FAKE_THING", "OPENAI_API_KEY") {
			if got[n] {
				t.Errorf("%s: %s reached claude", site, n)
			}
		}
		for _, n := range []string{"RYSH_PANE", "GIT_EDITOR", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "PATH"} {
			if !got[n] {
				t.Errorf("%s: KEEP %s missing for claude", site, n)
			}
		}
	}
	// codex is not verified on this host (no codex binary): its lines carry the
	// GENERATED static prefix, so the exact DENY names are gone and its own
	// OPENAI_API_KEY stays (patterns are applied once codex is verified).
	for site, line := range codexLines {
		got := runTyped(t, line, scrubFakeEnv())
		for _, n := range agentenv.StaticNames() {
			if got[n] {
				t.Errorf("%s: exact DENY %s reached codex", site, n)
			}
		}
		for _, n := range []string{"OPENAI_API_KEY", "RYSH_PANE", "PATH"} {
			if !got[n] {
				t.Errorf("%s: %s missing for codex", site, n)
			}
		}
	}
}

// agentCommandOf unwraps a scrubbed typed line to the AGENT command inside it
// (the part between exec-clean's "--" and its fallback), or strips the static
// prefix from a codex line. Older tests that pin the agent command's shape use
// it, so they keep testing exactly what they tested; that the line IS scrubbed
// is TestTypedAgentLinesScrubCredentials's job.
func agentCommandOf(line string) string {
	if i := strings.Index(line, " exec-clean --agent "); i >= 0 {
		rest := line[i+len(" exec-clean --agent "):]
		if j := strings.Index(rest, " -- "); j >= 0 {
			rest = rest[j+len(" -- "):]
		}
		if k := strings.Index(rest, " || test -x "); k >= 0 {
			rest = rest[:k]
		}
		return rest
	}
	return strings.TrimPrefix(line, agentenv.StaticPrefix()+" ")
}
