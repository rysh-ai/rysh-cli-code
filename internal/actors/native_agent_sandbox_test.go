// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/agentenv"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/sandbox"
)

// Design 066 S3: `##claude --sandbox <profile>`.

func TestStripSandboxFlag(t *testing.T) {
	cases := []struct {
		in       []string
		rest     string
		profile  string
		wantsErr bool
	}{
		{[]string{"--model", "opus"}, "--model opus", "", false},
		{[]string{"--sandbox", "box", "--model", "opus"}, "--model opus", "box", false},
		{[]string{"--model", "opus", "--sandbox=box"}, "--model opus", "box", false},
		{[]string{"--sandbox"}, "", "", true},
		{[]string{"--sandbox", "--model"}, "", "", true},
		{[]string{"--sandbox", "a", "--sandbox", "b"}, "", "", true},
		{[]string{"--sandbox", "Bad Name"}, "", "", true},
	}
	for _, tc := range cases {
		rest, prof, err := nativeAgentStripSandbox(tc.in)
		if (err != nil) != tc.wantsErr {
			t.Fatalf("%v: err = %v", tc.in, err)
		}
		if err == nil && (strings.Join(rest, " ") != tc.rest || prof != tc.profile) {
			t.Fatalf("%v: rest=%v prof=%q", tc.in, rest, prof)
		}
	}
}

func TestSandboxedCommandGoesThroughExecCleanSandboxOnly(t *testing.T) {
	agentenv.SetBinaryForTest("/opt/rysh/bin/rysh")
	line := nativeAgentCommand(nativeAgentSpec{
		Kind: nativeAgentClaude, SessionID: "s1",
		Sandbox: "box", SandboxRyshDir: "/w/.rysh", Workspace: "/w",
	})
	for _, want := range []string{
		"exec-clean --agent claude --sandbox 'box' --rysh-dir '/w/.rysh' --workspace '/w' -- claude --session-id s1",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("line missing %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "env -u") {
		t.Fatalf("sandboxed line has the unsandboxed static fallback:\n%s", line)
	}
}

func TestSandboxedCommandWithoutWorkspaceStartsNothing(t *testing.T) {
	agentenv.SetBinaryForTest("/opt/rysh/bin/rysh")
	for _, s := range []nativeAgentSpec{
		{Kind: nativeAgentClaude, SessionID: "s1", Sandbox: "box", SandboxRyshDir: "/w/.rysh"},
		{Kind: nativeAgentClaude, SessionID: "s1", Sandbox: "box", Workspace: "/w"},
		// gap01 E3: codex is supported; an incomplete codex spec still refuses.
		{Kind: nativeAgentCodex, Sandbox: "box", SandboxRyshDir: "/w/.rysh"},
	} {
		line := nativeAgentCommand(s)
		if strings.Contains(line, "--session-id") || strings.Contains(line, "exec-clean") || strings.Contains(line, "env -u") {
			t.Fatalf("an incomplete sandbox spec must start nothing:\n%s", line)
		}
		if !strings.Contains(line, "not started") {
			t.Fatalf("no explanation:\n%s", line)
		}
	}
}

func TestSandboxMetaRoundTripsIntoAResume(t *testing.T) {
	agentenv.SetBinaryForTest("/opt/rysh/bin/rysh")
	rysh := t.TempDir()
	sid := "6f1c9e2a-0000-4000-8000-00000000abcd"
	meta := map[string]string{}
	for _, kv := range nativeAgentMeta(nativeAgentClaude, "/work/p", "", sid, false, "") {
		meta[kv[0]] = kv[1]
	}
	for _, kv := range nativeAgentSandboxMeta("box", rysh) {
		meta[kv[0]] = kv[1]
	}
	if meta[metaAgentSandbox] != "box" || meta[metaAgentSandboxDir] != rysh {
		t.Fatalf("meta = %v", meta)
	}

	// No session in the SANDBOX home yet: the host check says yes, but the
	// sandboxed pane must be judged by its own home.
	deliveries, _, _ := nativeResumePlan([]ansaPane{agentPane("p1", meta)}, idleProbe, sessionAlwaysExists, nil)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	cmd := deliveries[0].Command
	for _, want := range []string{"--sandbox 'box'", "--workspace '/work/p'", "cd '/work/p'", "claude --session-id " + sid} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("resume missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "env -u") {
		t.Fatalf("resume has the unsandboxed fallback:\n%s", cmd)
	}

	// Once the sandbox home holds the session, the sweep resumes it.
	proj := filepath.Join(sandbox.HomeDir(rysh, "box"), ".claude", "projects", "-work-p")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, sid+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deliveries, _, _ = nativeResumePlan([]ansaPane{agentPane("p1", meta)}, idleProbe, func(string) bool { return false }, nil)
	if len(deliveries) != 1 || !strings.Contains(deliveries[0].Command, "claude --resume "+sid) {
		t.Fatalf("expected a sandboxed --resume, got %+v", deliveries)
	}
}

func TestSandboxedPaneWithLostRyshDirResumesNothingUnsandboxed(t *testing.T) {
	agentenv.SetBinaryForTest("/opt/rysh/bin/rysh")
	meta := map[string]string{
		metaAgentNative: "1", metaAgentKind: "claude", metaAgentCwd: "/work/p",
		metaAgentSandbox: "box", // agent.sandbox_rysh_dir lost
	}
	deliveries, _, _ := nativeResumePlan([]ansaPane{agentPane("p1", meta)}, idleProbe, sessionAlwaysExists, nil)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v", deliveries)
	}
	if c := deliveries[0].Command; strings.Contains(c, "--session-id") || strings.Contains(c, "exec-clean") || strings.Contains(c, "env -u") || !strings.Contains(c, "not started") {
		t.Fatalf("a sandboxed pane must never come back unsandboxed:\n%s", deliveries[0].Command)
	}
}

func TestPaneListSuffixShowsSandbox(t *testing.T) {
	got := paneListSuffix(domain.PaneSnapshot{Program: "claude", Meta: map[string]string{metaAgentSandbox: "box"}})
	if got != "  running=claude  sandbox=box" {
		t.Fatalf("suffix = %q", got)
	}
	if got := paneListSuffix(domain.PaneSnapshot{}); got != "" {
		t.Fatalf("suffix = %q", got)
	}
}
