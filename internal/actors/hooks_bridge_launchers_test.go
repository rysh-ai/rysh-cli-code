// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// B-3 (cli-core, 2026-11-01): with claude_bridge on, the lifecycle stops firing
// hooks for every claude pane from the screen (a bridged claude reports them
// itself). So a claude started WITHOUT the bridge got no hooks at all: the
// three launchers that skipped it were `##pane new --claude`, the board agent's
// fresh start and a fleet `##ansa resume`.
func TestClaudeBridge_OtherLaunchers(t *testing.T) {
	const bridge = "--settings '/w/.rysh/generated/claude-hooks.json'"
	setClaudeBridgeSettings("/w/.rysh/generated/claude-hooks.json")
	t.Cleanup(func() { setClaudeBridgeSettings("") })

	for name, line := range map[string]string{
		"##pane new --claude": claudeLaunchCommand("sid-1", "", ""),
		"board agent":         claudeLaunchCommand("sid-2", "/tmp/p.txt", boardAgentClaudeArgs),
		"##ansa resume":       mustResumeLine(t, fleetAgentPane("pane-c", "claude", ""), "claude", "pane-c"),
	} {
		// The typed line names claude twice (exec-clean, then the env -u
		// fallback); each must carry the bridge.
		if n := strings.Count(line, bridge); n == 0 || n != strings.Count(line, " -- claude ")+strings.Count(line, " CLAUDE_EFFORT claude ") {
			t.Errorf("%s: %d bridge flags for its claude invocations: %s", name, n, line)
		}
	}

	// Never next to a --settings of the caller's own; never for codex.
	own := fleetAgentPane("pane-o", "claude", "")
	own.Meta["agent.args"] = "--settings /mine.json"
	for name, line := range map[string]string{
		"launch, own --settings": claudeLaunchCommand("sid-3", "", "--settings=/mine.json"),
		"resume, own --settings": mustResumeLine(t, own, "claude", "pane-o"),
		"codex resume":           mustResumeLine(t, fleetAgentPane("pane-x", "codex", ""), "codex", ""),
	} {
		if strings.Contains(line, "claude-hooks.json") {
			t.Errorf("%s must not get the bridge: %s", name, line)
		}
	}

	setClaudeBridgeSettings("")
	if line := claudeLaunchCommand("sid-1", "", ""); strings.Contains(line, "--settings") {
		t.Errorf("bridge off, still injected: %s", line)
	}
	if line := mustResumeLine(t, fleetAgentPane("pane-c", "claude", ""), "claude", "pane-c"); strings.Contains(line, "--settings") {
		t.Errorf("bridge off, still injected on resume: %s", line)
	}
}

// A pane stamped agent.sandbox runs its agent under a sandbox profile. `##ansa
// resume` builds a bare `claude --resume` / `codex resume` line with no sandbox
// wrapper, so resuming such a pane would start its agent unsandboxed. It is
// refused; the native resume (`##claude --resume`, the daemon restart path)
// re-applies the profile.
func TestAnsaResume_RefusesSandboxedPane(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		p := fleetAgentPane("pane-s", agent, "")
		p.Meta[metaAgentSandbox] = "strict"
		line, err := ansaResumeLine(p, agent, "pane-s")
		if err == nil || !strings.Contains(err.Error(), "sandbox") {
			t.Errorf("%s: a sandboxed pane resumed unsandboxed: %q, %v", agent, line, err)
		}
	}
}
