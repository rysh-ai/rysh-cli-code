// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/hooks"
)

func TestNativeAgentCommand_ClaudeBridge(t *testing.T) {
	setClaudeBridgeSettings("/w/.rysh/generated/claude-hooks.json")
	t.Cleanup(func() { setClaudeBridgeSettings("") })

	got := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: "s"})
	if !strings.Contains(got, "--settings '/w/.rysh/generated/claude-hooks.json'") {
		t.Fatalf("claude launch lacks the bridge: %s", got)
	}
	got = nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: "s", Resume: true})
	if !strings.Contains(got, "--settings '/w/.rysh/generated/claude-hooks.json'") {
		t.Fatalf("claude resume lacks the bridge: %s", got)
	}
	for name, spec := range map[string]nativeAgentSpec{
		"codex":            {Kind: nativeAgentCodex},
		"own --settings":   {Kind: nativeAgentClaude, Args: "--settings /f.json"},
		"own --settings=":  {Kind: nativeAgentClaude, Args: "--settings=/f.json"},
		"sandboxed claude": {Kind: nativeAgentClaude, Sandbox: "strict", SandboxRyshDir: "/w/.rysh", Workspace: "/w"},
	} {
		if got := nativeAgentCommand(spec); strings.Contains(got, "claude-hooks.json") {
			t.Errorf("%s must not get the bridge: %s", name, got)
		}
	}
	setClaudeBridgeSettings("")
	if got := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude}); strings.Contains(got, "--settings") {
		t.Fatalf("bridge off, still injected: %s", got)
	}
}

func hooksWorkspace(t *testing.T, body string) *WorkspaceActor {
	t.Helper()
	dir := t.TempDir()
	rysh := filepath.Join(dir, ".rysh")
	if err := os.MkdirAll(rysh, 0o755); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if err := os.WriteFile(filepath.Join(rysh, hooks.FileName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w := &WorkspaceActor{cfg: config.Config{RyshDir: rysh, WorkingDirectory: dir}}
	t.Cleanup(func() {
		setClaudeBridgeSettings("")
		setHooksBroken(nil)
		setLifecycle(nil)
		sharedagentic.SetApprovalPolicy(nil)
	})
	return w
}

func TestHooksCommand_ListReloadAndBridgeFile(t *testing.T) {
	w := hooksWorkspace(t, "claude_bridge: true\nhooks:\n  on-finish:\n    - run: ./lint.sh\n")
	w.loadHooks()
	var out strings.Builder
	if err := w.handleHooksCommand(&out, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"on-finish", "./lint.sh", "claude bridge: on", "checkpoints: auto"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list lacks %q:\n%s", want, out.String())
		}
	}
	path := claudeBridgeSettings()
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "hook emit --rysh-dir") {
		t.Fatalf("bridge settings %s: %v %s", path, err, b)
	}

	// Broken file on reload: reported, and the bridge stays on so the gate
	// keeps failing closed instead of silently vanishing.
	if err := os.WriteFile(filepath.Join(w.cfg.RyshDir, hooks.FileName), []byte("hookz: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := w.handleHooksCommand(&out, []string{"reload"}); err == nil || !strings.Contains(out.String(), "pre-tool calls are blocked") {
		t.Fatalf("reload of a broken file: %v\n%s", err, out.String())
	}
	if claudeBridgeSettings() == "" {
		t.Fatal("a broken file must not switch the bridge off")
	}
	out.Reset()
	if err := w.handleHooksCommand(&out, []string{"wibble"}); err == nil || !strings.Contains(out.String(), "usage") {
		t.Fatalf("unknown subcommand: %v %q", err, out.String())
	}
}

func TestHooks_PreToolGatesNativeAgent(t *testing.T) {
	w := hooksWorkspace(t, "hooks:\n  pre-tool:\n    - run: 'exit 1'\n      match: '^bash$'\n")
	w.loadHooks()
	f := w.approvalPolicyFunc()
	if f == nil {
		t.Fatal("pre-tool hooks must install an approval policy even without policy.yaml")
	}
	dec, rule := f("bash", json.RawMessage(`{"command":"ls"}`))
	if dec != sharedagentic.PolicyGate || rule != "hooks.pre-tool" {
		t.Fatalf("failing pre-tool hook: dec=%v rule=%q", dec, rule)
	}
	if dec, _ := f("read_file", nil); dec != sharedagentic.PolicyDefault {
		t.Fatalf("unmatched tool: %v", dec)
	}
}

func TestLifecycle_AgentStartAndNeedsYouCheckpoint(t *testing.T) {
	repo := checkpointRepo(t)
	w := hooksWorkspace(t, "")
	w.loadHooks()
	l := currentLifecycle()
	if l == nil {
		t.Fatal("no lifecycle installed")
	}
	l.handle(context.Background(), paneEvent{pane: "p1", kind: paneEventAgentStart, program: "claude", dir: repo})
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.handle(context.Background(), paneEvent{pane: "p1", kind: paneEventNeedsYou, program: "claude", dir: repo, reason: "prompt"})
	var out strings.Builder
	if err := runCheckpointCommand(context.Background(), &out, repo, "p1", []string{"list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "#2") || !strings.Contains(out.String(), "agent start") || !strings.Contains(out.String(), "needs you") {
		t.Fatalf("checkpoints:\n%s", out.String())
	}
	// Not an agent CLI: nothing.
	l.handle(context.Background(), paneEvent{pane: "p2", kind: paneEventAgentStart, program: "vim", dir: repo})
	out.Reset()
	_ = runCheckpointCommand(context.Background(), &out, repo, "p2", nil)
	if !strings.Contains(out.String(), "no checkpoints") {
		t.Fatalf("vim got a checkpoint:\n%s", out.String())
	}
}

func TestLifecycle_HooksFireAndBridgeDedup(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen")
	w := hooksWorkspace(t, "checkpoints: off\nhooks:\n  on-needs-you:\n    - run: 'cat >> "+seen+"'\n  on-finish:\n    - run: 'cat >> "+seen+"'\n")
	w.loadHooks()
	l := currentLifecycle()
	ctx := context.Background()
	l.handle(ctx, paneEvent{pane: "p1", kind: paneEventNeedsYou, program: "codex", reason: "prompt"})
	l.handle(ctx, paneEvent{pane: "p1", kind: paneEventFinish, program: "codex"})
	b, _ := os.ReadFile(seen)
	if !strings.Contains(string(b), `"event":"on-needs-you"`) || !strings.Contains(string(b), `"event":"on-finish"`) {
		t.Fatalf("hooks saw %q", b)
	}

	// With the bridge on, claude reports these itself; the screen-sourced
	// copies would fire every hook twice.
	_ = os.Remove(seen)
	w2 := hooksWorkspace(t, "claude_bridge: true\nhooks:\n  on-needs-you:\n    - run: 'cat >> "+seen+"'\n")
	w2.loadHooks()
	currentLifecycle().handle(ctx, paneEvent{pane: "p1", kind: paneEventNeedsYou, program: "claude"})
	if _, err := os.Stat(seen); err == nil {
		t.Fatal("bridged claude: the screen-sourced on-needs-you must not fire")
	}
}

// A sandboxed pane's agent can write the workspace, where the hook scripts
// live; the host must never run them on its behalf (lead ruling R3). Checkpoints
// still run: they only read the tree, through hardened git.
func TestLifecycle_SandboxedPaneRunsNoHooks(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen")
	repo := checkpointRepo(t)
	w := hooksWorkspace(t, "hooks:\n  on-needs-you:\n    - run: 'cat >> "+seen+"'\n  on-finish:\n    - run: 'cat >> "+seen+"'\n")
	w.loadHooks()
	l := currentLifecycle()
	ctx := context.Background()
	for _, prog := range []string{"claude", "codex"} {
		l.handle(ctx, paneEvent{pane: "p1", kind: paneEventNeedsYou, program: prog, reason: "prompt", dir: repo, sandboxed: true})
		l.handle(ctx, paneEvent{pane: "p1", kind: paneEventFinish, program: prog, dir: repo, sandboxed: true})
	}
	if b, err := os.ReadFile(seen); err == nil {
		t.Fatalf("a sandboxed pane ran host hooks: %q", b)
	}
	var out strings.Builder
	if err := runCheckpointCommand(ctx, &out, repo, "p1", []string{"list"}); err != nil || !strings.Contains(out.String(), "needs you") {
		t.Fatalf("a sandboxed pane still gets its checkpoints: %v\n%s", err, out.String())
	}
	// The same events from an unsandboxed pane do run them.
	l.handle(ctx, paneEvent{pane: "p2", kind: paneEventFinish, program: "codex", dir: repo})
	if b, _ := os.ReadFile(seen); !strings.Contains(string(b), `"event":"on-finish"`) {
		t.Fatalf("unsandboxed pane: hooks saw %q", b)
	}
}

// A hooks.yaml that does not load is an unknown posture: claude is not started
// at all rather than started ungated (lead ruling R7). The launch paths that
// build their line with nativeAgentCommand refuse there, and ##claude refuses
// before touching the pane; TestBrokenHooksRefuseOtherClaudeLaunchers covers
// the rest.
func TestBrokenHooksRefuseClaudeLaunches(t *testing.T) {
	w := hooksWorkspace(t, "hookz: 1\n")
	w.loadHooks()
	for name, spec := range map[string]nativeAgentSpec{
		"launch":    {Kind: nativeAgentClaude, SessionID: "s"},
		"resume":    {Kind: nativeAgentClaude, SessionID: "s", Resume: true},
		"sandboxed": {Kind: nativeAgentClaude, Sandbox: "strict", SandboxRyshDir: "/w/.rysh", Workspace: "/w"},
	} {
		got := nativeAgentCommand(spec)
		if !strings.Contains(got, "hooks.yaml") || !strings.Contains(got, "not started") || strings.Contains(got, "--session-id") || strings.Contains(got, "--resume") {
			t.Errorf("%s with a broken hooks.yaml: %s", name, got)
		}
	}
	if got := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentCodex}); strings.Contains(got, "not started") {
		t.Errorf("codex has no hook gate to lose: %s", got)
	}
	// No transport: reaching the pane probe would panic, so an error proves the
	// refusal came first.
	var out strings.Builder
	if err := w.handleNativeAgentCommand(&out, promptTestPane, nativeAgentClaude, nil); err == nil ||
		!strings.Contains(out.String(), "hooks.yaml") || !strings.Contains(out.String(), "nothing was changed") {
		t.Fatalf("##claude with a broken hooks.yaml: %v\n%s", err, out.String())
	}

	// Fixed and reloaded: claude starts again.
	if err := os.WriteFile(filepath.Join(w.cfg.RyshDir, hooks.FileName), []byte("hooks: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.loadHooks()
	if got := nativeAgentCommand(nativeAgentSpec{Kind: nativeAgentClaude, SessionID: "s"}); !strings.Contains(got, "--session-id") {
		t.Fatalf("after a good reload: %s", got)
	}
}

// The claude launchers that do not go through nativeAgentCommand refuse too:
// `##pane new --claude` (before it opens a pane or writes a prompt), every
// line claudeLaunchCommand builds (the scheduled launches and the board
// agent's fresh start), and the fleet `##ansa resume` line.
func TestBrokenHooksRefuseOtherClaudeLaunchers(t *testing.T) {
	w := hooksWorkspace(t, "hookz: 1\n")
	w.loadHooks()

	var out strings.Builder
	w.handlePaneNewClaude(nil, &out, "", []string{"do", "the", "thing"}, false)
	if !strings.Contains(out.String(), "hooks.yaml") || !strings.Contains(out.String(), "nothing was changed") {
		t.Errorf("##pane new --claude with a broken hooks.yaml:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(w.baseDir(), ".rysh", "claude-panes")); err == nil {
		t.Error("##pane new --claude wrote a prompt file before refusing")
	}

	for name, line := range map[string]string{
		"launch":      claudeLaunchCommand("sid-1", "", ""),
		"board agent": claudeLaunchCommand("sid-2", "/tmp/p.txt", boardAgentClaudeArgs),
	} {
		if !strings.Contains(line, "hooks.yaml") || !strings.Contains(line, "not started") || strings.Contains(line, "--session-id") {
			t.Errorf("%s with a broken hooks.yaml: %s", name, line)
		}
	}

	if line, err := ansaResumeLine(fleetAgentPane("pane-c", "claude", ""), "claude", "pane-c"); err == nil ||
		!strings.Contains(err.Error(), "hooks.yaml") {
		t.Errorf("##ansa resume of a claude with a broken hooks.yaml: %q, %v", line, err)
	}
	if _, err := ansaResumeLine(fleetAgentPane("pane-x", "codex", ""), "codex", ""); err != nil {
		t.Errorf("codex has no hook gate to lose: %v", err)
	}

	// Fixed and reloaded: they start again.
	if err := os.WriteFile(filepath.Join(w.cfg.RyshDir, hooks.FileName), []byte("hooks: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.loadHooks()
	if line := claudeLaunchCommand("sid-1", "", ""); !strings.Contains(line, "--session-id") {
		t.Errorf("after a good reload: %s", line)
	}
	if _, err := ansaResumeLine(fleetAgentPane("pane-c", "claude", ""), "claude", "pane-c"); err != nil {
		t.Errorf("after a good reload: %v", err)
	}
}

// The sandbox stamp decides whether host hooks run for a pane, so the pane
// meta command may neither set nor delete it (nor its sibling keys): deleting
// it would put host hooks back on for a sandboxed agent. A bare workspace has
// no transport, so reaching the send would panic.
func TestPaneMetaRefusesTheSandboxStamp(t *testing.T) {
	w := &WorkspaceActor{}
	for _, args := range [][]string{
		{"meta", "delete", metaAgentSandbox},
		{"meta", "rm", metaAgentSandboxDir},
		{"meta", "set", metaAgentSandbox, "strict"},
		{"meta", "set", "agent.sandbox.x", "y"},
	} {
		var out strings.Builder
		w.handlePaneCommand(nil, &out, "pane-1", args)
		if !strings.Contains(out.String(), "reserved") {
			t.Errorf("##pane %s: %s", strings.Join(args, " "), out.String())
		}
	}
}
