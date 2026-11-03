// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/agentenv"
	"github.com/rysh-ai/rysh-cli-code/internal/sandbox"
)

// gap01 E4: sandbox by default, and sandbox required.

func TestStripNoSandboxFlag(t *testing.T) {
	rest, no := nativeAgentStripNoSandbox([]string{"--model", "opus", "--no-sandbox"})
	if !no || strings.Join(rest, " ") != "--model opus" {
		t.Fatalf("rest=%v no=%v", rest, no)
	}
	if _, no := nativeAgentStripNoSandbox([]string{"--model", "opus"}); no {
		t.Fatal("no flag, but no-sandbox reported")
	}
}

func TestResolveNativeAgentSandbox(t *testing.T) {
	broken := errors.New("sandbox.yaml: broken")
	cases := []struct {
		name         string
		flag, sticky string
		noSandbox    bool
		set          sandbox.Settings
		setErr       error
		want         string
		wantErr      string
	}{
		{name: "opt-in, nothing asked", want: ""},
		{name: "flag wins", flag: "box", set: sandbox.Settings{Default: "dflt"}, want: "box"},
		{name: "sticky wins over default", sticky: "old", set: sandbox.Settings{Default: "dflt"}, want: "old"},
		{name: "default applies", set: sandbox.Settings{Default: "dflt"}, want: "dflt"},
		{name: "no-sandbox opts out", noSandbox: true, set: sandbox.Settings{Default: "dflt"}, want: ""},
		{name: "no-sandbox drops sticky", noSandbox: true, sticky: "old", want: ""},
		{name: "required uses default", set: sandbox.Settings{Default: "dflt", Required: true}, want: "dflt"},
		{name: "required, no default, no flag", set: sandbox.Settings{Required: true}, wantErr: "requires a sandbox"},
		{name: "required refuses no-sandbox", noSandbox: true, set: sandbox.Settings{Default: "dflt", Required: true}, wantErr: "requires a sandbox"},
		{name: "required satisfied by flag", flag: "box", set: sandbox.Settings{Required: true}, want: "box"},
		{name: "both flags", flag: "box", noSandbox: true, wantErr: "--no-sandbox"},
		{name: "broken settings fail closed", setErr: broken, wantErr: "broken"},
		{name: "broken settings even with a flag", flag: "box", setErr: broken, wantErr: "broken"},
	}
	for _, tc := range cases {
		got, err := resolveNativeAgentSandbox(tc.flag, tc.noSandbox, tc.sticky, tc.set, tc.setErr)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q err %v, want %q", tc.name, got, err, tc.want)
		}
	}
}

// The restore sweep honours required: an unsandboxed agent pane comes back in
// the default profile, or does not come back at all.
func TestResumeHonoursRequiredSandbox(t *testing.T) {
	agentenv.SetBinaryForTest("/opt/rysh/bin/rysh")
	meta := map[string]string{metaAgentNative: "1", metaAgentKind: "claude", metaAgentCwd: "/work/p"}
	panes := []ansaPane{agentPane("p1", meta)}

	d, _, _ := nativeResumePlanWith(panes, idleProbe, sessionAlwaysExists, nil, nativeSandboxPolicy{RyshDir: "/work/p/.rysh", Settings: sandbox.Settings{Default: "dflt", Required: true}})
	if len(d) != 1 || !strings.Contains(d[0].Command, "--sandbox 'dflt'") || strings.Contains(d[0].Command, "env -u") {
		t.Fatalf("required+default: %+v", d)
	}
	// The host conversation is not in the sandbox home: no --resume of it.
	if strings.Contains(d[0].Command, "--resume") {
		t.Fatalf("resumed a host conversation inside the sandbox:\n%s", d[0].Command)
	}

	d, _, _ = nativeResumePlanWith(panes, idleProbe, sessionAlwaysExists, nil, nativeSandboxPolicy{Settings: sandbox.Settings{Required: true}})
	if len(d) != 1 || strings.Contains(d[0].Command, "exec-clean") || strings.Contains(d[0].Command, "env -u") || !strings.Contains(d[0].Command, "not started") {
		t.Fatalf("required without default must start nothing: %+v", d)
	}

	d, _, _ = nativeResumePlanWith(panes, idleProbe, sessionAlwaysExists, nil, nativeSandboxPolicy{Err: errors.New("sandbox.yaml: broken")})
	if len(d) != 1 || !strings.Contains(d[0].Command, "not started") {
		t.Fatalf("broken settings must start nothing: %+v", d)
	}

	// Opt-in workspaces are unchanged: default alone does not convert a pane
	// the user started unsandboxed on restore.
	d, _, _ = nativeResumePlanWith(panes, idleProbe, sessionAlwaysExists, nil, nativeSandboxPolicy{Settings: sandbox.Settings{Default: "dflt"}})
	if len(d) != 1 || strings.Contains(d[0].Command, "--sandbox") {
		t.Fatalf("default without required changed a restore: %+v", d)
	}
}

// gap01 E3: ##codex --sandbox.
func TestCodexSandboxedCommand(t *testing.T) {
	agentenv.SetBinaryForTest("/opt/rysh/bin/rysh")
	line := nativeAgentCommand(nativeAgentSpec{
		Kind: nativeAgentCodex, SessionID: "c1", Resume: true,
		Sandbox: "box", SandboxRyshDir: "/w/.rysh", Workspace: "/w",
	})
	if !strings.Contains(line, "exec-clean --agent codex --sandbox 'box' --rysh-dir '/w/.rysh' --workspace '/w' -- codex") {
		t.Fatalf("line:\n%s", line)
	}
	if strings.Contains(line, "env -u") || strings.HasPrefix(line, "printf") {
		t.Fatalf("sandboxed codex line:\n%s", line)
	}
	// A codex conversation id is discovered in the HOST ~/.codex; inside the
	// sandbox it would name nothing, so a sandboxed codex never resumes one.
	if strings.Contains(line, "resume") {
		t.Fatalf("sandboxed codex resumed a host session:\n%s", line)
	}
	w := &WorkspaceActor{}
	if err := w.checkNativeAgentSandbox(nativeAgentCodex, "box", "/w", false, false); err != nil && strings.Contains(err.Error(), "claude only") {
		t.Fatalf("codex still refused: %v", err)
	}
}
