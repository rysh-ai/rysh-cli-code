// SPDX-License-Identifier: Apache-2.0

package agentenv

// FAKE values only (root-0472 (c)): every value below is the literal "FAKE-…".
// The tests look at NAMES; no real credential is read, printed or compared.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// fakeEnv builds KEY=FAKE-KEY pairs for names (plus PATH/HOME so a child runs).
func fakeEnv(names ...string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent-fake-home"}
	for _, n := range names {
		env = append(env, n+"=FAKE-"+n)
	}
	return env
}

func namesOf(env []string) map[string]bool {
	m := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		m[k] = true
	}
	return m
}

// patternFakes are invented names, one per DENY pattern.
var patternFakes = []string{
	"AWS_FAKE_THING",     // AWS_*
	"FOO_PASSWORD",       // *_PASSWORD
	"FOO_TOKEN",          // *_TOKEN
	"ZZ_API_KEY",         // *_API_KEY
	"MY_PAT",             // *_PAT
	"SVC_CREDENTIALS",    // *_CREDENTIALS
	"DEPLOY_PRIVATE_KEY", // *_PRIVATE_KEY
	"SIGNING_PASSPHRASE", // *_PASSPHRASE
	"X_SECRET_Y",         // *_SECRET*
	"APP_SECRET",         // *_SECRET*
	"GIT_CONFIG_KEY_0",   // GIT_CONFIG_KEY_*
	"GIT_CONFIG_VALUE_0", // GIT_CONFIG_VALUE_*
}

// keepFakes pass for every agent: prefix KEEPs and ordinary names.
var keepFakes = []string{
	"RYSH_PANE", "RYSH_SESSION", "RYSH_WORKSPACE",
	"GIT_EDITOR", "GIT_AUTHOR_NAME", "GIT_ALLOW_PROTOCOL",
	"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "GEMINI_BASE_URL",
	"SSH_AUTH_SOCK", "TERM", "LANG",
	"SOME_KEY",        // no bare *_KEY pattern
	"MONKEY_BUSINESS", // contains KEY, not a deny pattern
}

func TestFilterDeniesEveryExactNameAndPattern(t *testing.T) {
	for _, kind := range []Kind{Claude, Codex} {
		env := fakeEnv(append(append(append([]string{}, StaticNames()...), patternFakes...), keepFakes...)...)
		got := namesOf(Filter(env, kind))
		for _, n := range StaticNames() {
			if got[n] {
				t.Errorf("%s: exact DENY %s survived", kind, n)
			}
		}
		for _, n := range patternFakes {
			if got[n] {
				t.Errorf("%s: pattern DENY %s survived", kind, n)
			}
		}
		for _, n := range append([]string{"PATH", "HOME"}, keepFakes...) {
			if !got[n] {
				t.Errorf("%s: KEEP %s was dropped", kind, n)
			}
		}
	}
}

// The precedence cases root-0472 (a)/(c) name explicitly.
func TestPrecedence(t *testing.T) {
	env := fakeEnv("RYSH_CLI_PAT", "RYSH_PANE", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_HOME", "GEMINI_API_KEY", "GIT_USERNAME",
		"GIT_ASKPASS", "GIT_CONFIG_COUNT", "GIT_EDITOR")
	claude := namesOf(Filter(env, Claude))
	codex := namesOf(Filter(env, Codex))
	cases := []struct {
		name          string
		claude, codex bool
	}{
		{"RYSH_CLI_PAT", false, false},           // prefix KEEP RYSH_* never rescues *_PAT
		{"RYSH_PANE", true, true},                // prefix KEEP
		{"ANTHROPIC_API_KEY", true, false},       // exact KEEP for claude beats *_API_KEY
		{"ANTHROPIC_AUTH_TOKEN", true, false},    // exact KEEP for claude beats *_TOKEN
		{"CLAUDE_CODE_OAUTH_TOKEN", true, false}, // claude's own login
		{"OPENAI_API_KEY", false, true},          // dropped for claude, kept for codex
		{"CODEX_HOME", true, true},               // not a deny name at all
		{"GEMINI_API_KEY", false, false},         // neither agent keeps it
		{"GIT_USERNAME", false, false},           // GIT_* denies (root-0472 (b))
		{"GIT_ASKPASS", false, false},
		{"GIT_CONFIG_COUNT", false, false},
		{"GIT_EDITOR", true, true}, // the rest of GIT_* is kept
	}
	for _, c := range cases {
		if claude[c.name] != c.claude {
			t.Errorf("claude: %s kept=%v, want %v", c.name, claude[c.name], c.claude)
		}
		if codex[c.name] != c.codex {
			t.Errorf("codex: %s kept=%v, want %v", c.name, codex[c.name], c.codex)
		}
	}
}

// Bedrock/Vertex users authenticate claude with AWS/Google names: kept only
// while the switch is set.
func TestProviderSwitchKeeps(t *testing.T) {
	base := []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_PROFILE", "GOOGLE_APPLICATION_CREDENTIALS"}
	off := namesOf(Filter(fakeEnv(base...), Claude))
	for _, n := range base {
		if off[n] {
			t.Errorf("switch off: %s kept", n)
		}
	}
	on := namesOf(Filter(append(fakeEnv(base...), "CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1"), Claude))
	for _, n := range base {
		if !on[n] {
			t.Errorf("switch on: %s dropped", n)
		}
	}
	if namesOf(Filter(append(fakeEnv(base...), "CLAUDE_CODE_USE_BEDROCK=1"), Codex))["AWS_ACCESS_KEY_ID"] {
		t.Error("codex must never keep claude's Bedrock names")
	}
}

// (d): the static fallback is GENERATED from the one list.
func TestStaticPrefixEqualsTheList(t *testing.T) {
	want := "env"
	for _, n := range StaticNames() {
		want += " -u " + n
	}
	if got := StaticPrefix(); got != want {
		t.Fatalf("StaticPrefix drifted from StaticNames:\n got %s\nwant %s", got, want)
	}
	seen := map[string]bool{}
	for _, n := range StaticNames() {
		if seen[n] {
			t.Errorf("duplicate static name %s", n)
		}
		seen[n] = true
		if !Denied(n) {
			t.Errorf("static name %s is not Denied()", n)
		}
	}
	if strings.ContainsAny(StaticPrefix(), "=$`") || strings.Contains(StaticPrefix(), "FAKE-") {
		t.Fatalf("static prefix carries something other than names: %s", StaticPrefix())
	}
}

func TestDroppedListsNamesOnly(t *testing.T) {
	env := fakeEnv("FOO_TOKEN", "RYSH_PANE", "AWS_SECRET_ACCESS_KEY")
	got := Dropped(env, Claude)
	want := []string{"AWS_SECRET_ACCESS_KEY", "FOO_TOKEN"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Dropped = %v, want %v", got, want)
	}
	for _, n := range got {
		if strings.Contains(n, "=") || strings.Contains(n, "FAKE-") {
			t.Fatalf("Dropped leaked a value: %q", n)
		}
	}
}

// Main parses, filters and hands the exec exactly the filtered env.
func TestMainExecsWithTheFilteredEnv(t *testing.T) {
	var gotPath string
	var gotArgv, gotEnv []string
	fake := func(p string, argv, env []string) error { gotPath, gotArgv, gotEnv = p, argv, env; return nil }
	env := fakeEnv("FOO_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "RYSH_PANE")
	var stderr bytes.Buffer
	rc := Main([]string{"--agent", "codex", "--explain", "--", "/bin/echo", "hi"}, env, &bytes.Buffer{}, &stderr, fake)
	if rc != 0 || gotPath != "/bin/echo" || strings.Join(gotArgv, " ") != "/bin/echo hi" {
		t.Fatalf("rc=%d path=%q argv=%v", rc, gotPath, gotArgv)
	}
	n := namesOf(gotEnv)
	if n["FOO_TOKEN"] || n["ANTHROPIC_API_KEY"] || !n["OPENAI_API_KEY"] || !n["RYSH_PANE"] {
		t.Fatalf("codex env names wrong: %v", n)
	}
	if strings.Contains(stderr.String(), "FAKE-") || !strings.Contains(stderr.String(), "FOO_TOKEN") {
		t.Fatalf("--explain must print dropped NAMES only: %q", stderr.String())
	}
	var out bytes.Buffer
	if rc := Main([]string{"--print-static"}, nil, &out, &bytes.Buffer{}, fake); rc != 0 || strings.TrimSpace(out.String()) != StaticPrefix() {
		t.Fatalf("--print-static rc=%d out=%q", rc, out.String())
	}
	if rc := Main([]string{"--agent", "nope", "--", "x"}, env, &bytes.Buffer{}, &bytes.Buffer{}, fake); rc != 2 {
		t.Fatalf("unknown agent rc=%d", rc)
	}
}

// ---- the real exec: the same pid, and the child sees names only ------------

// probe writes the stub child: an ABSOLUTE path, NOT named claude/codex
// (rr-msg-181). It prints its pid and its env NAMES (never a value).
func probe(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent-env-probe")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho \"PID=$$\"\nenv | cut -d= -f1 | sort\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMain lets this test binary BE `exec-clean` for the real-exec test.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "agentenv-exec-clean" {
		os.Exit(Main(os.Args[2:], os.Environ(), os.Stdout, os.Stderr, SysExec))
	}
	os.Exit(m.Run())
}

func TestRealExecKeepsThePidAndDropsTheNames(t *testing.T) {
	stub := probe(t)
	all := append(append(append([]string{}, StaticNames()...), patternFakes...), keepFakes...)
	all = append(all, "ANTHROPIC_API_KEY", "OPENAI_API_KEY")
	cmd := exec.Command(os.Args[0], "agentenv-exec-clean", "--agent", "claude", "--", stub)
	cmd.Env = fakeEnv(all...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("exec-clean: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if !strings.HasPrefix(lines[0], "PID=") {
		t.Fatalf("no pid line: %q", lines[0])
	}
	if pid, _ := strconv.Atoi(strings.TrimPrefix(lines[0], "PID=")); pid != cmd.Process.Pid {
		t.Fatalf("the agent ran as pid %d, exec-clean was %d: something lingered", pid, cmd.Process.Pid)
	}
	got := map[string]bool{}
	for _, l := range lines[1:] {
		if strings.Contains(l, "=") || strings.Contains(l, "FAKE-") {
			t.Fatalf("the probe printed more than a name: %q", l)
		}
		got[l] = true
	}
	for _, n := range append(append([]string{}, StaticNames()...), patternFakes...) {
		if got[n] {
			t.Errorf("DENY %s reached the agent", n)
		}
	}
	if got["OPENAI_API_KEY"] {
		t.Error("OPENAI_API_KEY reached claude")
	}
	for _, n := range append([]string{"ANTHROPIC_API_KEY"}, keepFakes...) {
		if !got[n] {
			t.Errorf("KEEP %s missing in the agent", n)
		}
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("agent env NAMES (%d): %s", len(names), strings.Join(names, " "))
}

func TestTypedLineShape(t *testing.T) {
	SetBinaryForTest("/opt/rysh test/rysh")
	line := TypedLine(Claude, "claude --resume abc")
	want := "test -x '/opt/rysh test/rysh' && '/opt/rysh test/rysh' exec-clean --agent claude -- claude --resume abc" +
		" || test -x '/opt/rysh test/rysh' || " + StaticPrefix() + " claude --resume abc"
	if line != want {
		t.Fatalf("TypedLine\n got %s\nwant %s", line, want)
	}
	if strings.Contains(line, "=") || strings.Contains(line, "FAKE-") {
		t.Fatalf("typed line carries a value: %s", line)
	}
}

// FilterStatic / ForRunner: the daemon-side exec paths.
func TestFilterStaticAndForRunner(t *testing.T) {
	env := fakeEnv(append([]string{"FOO_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "RYSH_PANE"}, StaticNames()...)...)
	st := namesOf(FilterStatic(env))
	for _, n := range StaticNames() {
		if st[n] {
			t.Errorf("FilterStatic kept exact %s", n)
		}
	}
	if !st["FOO_TOKEN"] || !st["OPENAI_API_KEY"] || !st["RYSH_PANE"] {
		t.Error("FilterStatic must drop exact names only")
	}
	cl := namesOf(ForRunner(env, "claude"))
	if cl["FOO_TOKEN"] || cl["OPENAI_API_KEY"] || !cl["ANTHROPIC_API_KEY"] {
		t.Error("ForRunner(claude) must be the full claude Filter")
	}
	if cx := namesOf(ForRunner(env, "codex")); !cx["FOO_TOKEN"] || !cx["OPENAI_API_KEY"] || cx["AWS_ACCESS_KEY_ID"] {
		t.Error("ForRunner(codex) must be FilterStatic until codex is verified")
	}
}
