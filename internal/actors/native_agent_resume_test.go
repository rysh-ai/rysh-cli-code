// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"strings"
	"testing"
)

// Design 029, the post-restore sweep. These are the properties a stop/start
// exercises and a detach/attach never will, so they are the ones a unit suite
// has to hold — the failure they guard against (an agent silently not coming
// back, or coming back twice) is invisible until someone reads the pane.

// idleProbe is a session where every pane sits at its shell — a freshly
// restored session, before the sweep runs.
func idleProbe(string) (string, error) { return "", nil }

// sessionAlwaysExists is the ordinary case: the agent's conversation is still
// on disk where it left it.
func sessionAlwaysExists(string) bool { return true }

// agentPane is a pane with arbitrary metadata. Deliberately NOT called
// nativePane: that name is taken, and taken for the OTHER meaning of native —
// a pane running rysh's own in-daemon agent (`--agent rysh`), which has no
// subprocess at all. These tests are about the opposite case, a claude or codex
// process rysh launched and must relaunch.
func agentPane(id string, meta map[string]string) ansaPane {
	return ansaPane{ID: id, Meta: meta}
}

// THE BOUNDARY OF THE WHOLE FEATURE. A pane where the user typed `claude` at
// their own shell carries no agent.native stamp, and the sweep must leave it
// alone. Resurrecting hand-started programs would make a session restart do
// things nobody asked it to.
func TestOnlyRyshLaunchedAgentsAreResumed(t *testing.T) {
	panes := []ansaPane{
		agentPane("rysh-launched", map[string]string{
			metaAgentNative: "1", metaAgentKind: "claude", metaAgentCwd: "/work",
		}),
		agentPane("hand-typed", map[string]string{}),
		agentPane("a-plain-shell", nil),
		// A fleet pane: fleetctl stamps fleet.agent, never agent.native. It is
		// resumed by `##ansa resume` on purpose, so the sweep must not also
		// drive it — two resumes of one session id is the bug this avoids.
		agentPane("fleet-worker", map[string]string{"fleet.agent": "claude", "fleet.role": "worker"}),
	}

	deliveries, _, _ := nativeResumePlan(panes, idleProbe, sessionAlwaysExists, nil)

	if len(deliveries) != 1 || deliveries[0].PaneID != "rysh-launched" {
		t.Fatalf("only the rysh-launched pane may be resumed, got %+v", deliveries)
	}
}

// The command a restored claude pane gets: into its own directory, resuming its
// own id. Both halves matter — claude stores sessions per project directory, so
// the right id in the wrong directory finds nothing.
func TestARestoredClaudePaneResumesItsOwnSessionInItsOwnDirectory(t *testing.T) {
	panes := []ansaPane{agentPane("pane-uuid-1", map[string]string{
		metaAgentNative: "1",
		metaAgentKind:   "claude",
		metaAgentCwd:    "/Users/x/project",
		metaAgentArgs:   "--dangerously-skip-permissions",
	})}

	deliveries, _, _ := nativeResumePlan(panes, idleProbe, sessionAlwaysExists, nil)
	if len(deliveries) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(deliveries))
	}
	cmd := deliveries[0].Command

	if !strings.HasPrefix(cmd, "cd '/Users/x/project' && ") {
		t.Errorf("must cd into the agent's directory: %q", cmd)
	}
	// No stamped id: the pane id IS the session id (fleetctl's invariant).
	if !strings.Contains(cmd, "claude --resume pane-uuid-1") {
		t.Errorf("must resume the pane's own session: %q", cmd)
	}
	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Errorf("a resumed agent must come back in the mode it was started in: %q", cmd)
	}
}

// A restored codex pane resumes the session discovery found for it, by id — not
// by `--last`, which cannot tell two agents in one directory apart.
func TestARestoredCodexPaneResumesTheDiscoveredSession(t *testing.T) {
	panes := []ansaPane{agentPane("pane-2", map[string]string{
		metaAgentNative:    "1",
		metaAgentKind:      "codex",
		metaAgentCwd:       "/srv/app",
		metaCodexSessionID: "019e60f7-d058-7ac1-9cd0-eb7be5280af2",
	})}

	deliveries, _, _ := nativeResumePlan(panes, idleProbe, sessionAlwaysExists, nil)
	if len(deliveries) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(deliveries))
	}
	want := "cd '/srv/app' && codex resume " + codexAutonomyFlag + " 019e60f7-d058-7ac1-9cd0-eb7be5280af2"
	if deliveries[0].Command != want {
		t.Errorf("got %q, want %q", deliveries[0].Command, want)
	}
}

// THE SWEEP MUST NOT NUDGE. `##ansa resume` passes a prompt telling a killed
// agent to carry on working; a session restart is not that. The founder's
// framing was "just bring their last session live, that is it" — a nudge here
// would have every restored pane start working, and spending, on startup.
func TestTheRestoreSweepDoesNotTellTheAgentToStartWorking(t *testing.T) {
	panes := []ansaPane{agentPane("p", map[string]string{
		metaAgentNative: "1", metaAgentKind: "claude", metaAgentCwd: "/w",
	})}

	deliveries, _, _ := nativeResumePlan(panes, idleProbe, sessionAlwaysExists, nil)
	if len(deliveries) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(deliveries))
	}
	if strings.Contains(deliveries[0].Command, ansaResumeNudge) {
		t.Errorf("the restore sweep must carry no nudge: %q", deliveries[0].Command)
	}
}

// A codex pane whose id was never discovered has NO conversation to bring back
// — codex records a session when the talking starts, so a missing id means the
// human never sent a first message. The sweep must start it fresh rather than
// fall back to `--last`, which would attach the pane to whatever ran in that
// directory most recently: a conversation nobody asked for, on a sweep nobody
// triggered.
func TestACodexPaneWithNoDiscoveredSessionStartsFreshRatherThanGuessing(t *testing.T) {
	panes := []ansaPane{agentPane("p", map[string]string{
		metaAgentNative: "1", metaAgentKind: "codex", metaAgentCwd: "/w",
	})}

	deliveries, _, _ := nativeResumePlan(panes, idleProbe, sessionAlwaysExists, nil)
	if len(deliveries) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(deliveries))
	}
	if got := deliveries[0].Command; got != "cd '/w' && codex "+codexAutonomyFlag {
		t.Errorf("must start codex fresh, got %q", got)
	}
}

// A claude whose session file is GONE must be relaunched under its own id, not
// resumed. `claude --resume <id>` on a session that no longer exists fails at
// the pane and leaves nothing running; re-pinning the id starts a working agent
// and keeps the pane's identity, which is the recoverable half of a bad
// situation. Reachable by deleting `~/.claude`, or by moving a session's
// project between machines.
func TestAClaudeWhoseSessionFileIsGoneIsRelaunchedRatherThanResumed(t *testing.T) {
	panes := []ansaPane{agentPane("pane-9", map[string]string{
		metaAgentNative: "1", metaAgentKind: "claude", metaAgentCwd: "/w",
	})}
	noSessions := func(string) bool { return false }

	deliveries, _, _ := nativeResumePlan(panes, idleProbe, noSessions, nil)
	if len(deliveries) != 1 {
		t.Fatalf("want 1 delivery, got %d", len(deliveries))
	}
	cmd := deliveries[0].Command
	if strings.Contains(cmd, "--resume") {
		t.Errorf("a session that does not exist must not be resumed: %q", cmd)
	}
	if !strings.Contains(cmd, "--session-id pane-9") {
		t.Errorf("the pane must keep its identity: %q", cmd)
	}
}

// A pane already running something is left alone — and marked settled, so later
// passes stop reconsidering it. This is the guard that keeps a retrying sweep
// from starting a second agent on top of the one it started a second ago.
func TestAPaneAlreadyRunningAnAgentIsNotResumedAgain(t *testing.T) {
	panes := []ansaPane{agentPane("busy", map[string]string{
		metaAgentNative: "1", metaAgentKind: "claude", metaAgentCwd: "/w",
	})}
	running := func(string) (string, error) { return "claude", nil }

	deliveries, settled, pending := nativeResumePlan(panes, running, sessionAlwaysExists, nil)
	if len(deliveries) != 0 {
		t.Errorf("a pane running an agent must not be resumed on top of it: %+v", deliveries)
	}
	if len(settled) != 1 || settled[0] != "busy" {
		t.Errorf("a running pane must be settled, got %v", settled)
	}
	if pending != 0 {
		t.Errorf("a pane that answered is not pending, got %d", pending)
	}
}

// The done set is the same guard across passes: once a pane has been delivered
// to, a later sweep must skip it even if its agent has not taken the terminal
// yet. Without this there is a window — deliver, retry a second later, probe
// still shows a shell — where one session id gets two agents.
func TestASecondPassDoesNotResumeAPaneTheFirstPassAlreadyDid(t *testing.T) {
	panes := []ansaPane{agentPane("p1", map[string]string{
		metaAgentNative: "1", metaAgentKind: "claude", metaAgentCwd: "/w",
	})}

	first, _, _ := nativeResumePlan(panes, idleProbe, sessionAlwaysExists, nil)
	if len(first) != 1 {
		t.Fatalf("first pass should deliver once, got %d", len(first))
	}

	done := map[string]bool{"p1": true}
	// The agent has not claimed the terminal yet — the probe still reports a
	// bare shell, exactly the racy window.
	second, _, _ := nativeResumePlan(panes, idleProbe, sessionAlwaysExists, done)
	if len(second) != 0 {
		t.Errorf("a pane already delivered to must not be resumed twice: %+v", second)
	}
}

// A pane that cannot be probed has not come up yet. It must be counted as
// pending — which is what schedules another pass — rather than skipped, or an
// agent whose pane was slow to start never comes back at all.
func TestAPaneThatCannotBeProbedIsRetriedRatherThanSkipped(t *testing.T) {
	panes := []ansaPane{
		agentPane("slow", map[string]string{metaAgentNative: "1", metaAgentKind: "claude"}),
		agentPane("ready", map[string]string{metaAgentNative: "1", metaAgentKind: "claude"}),
	}
	probe := func(id string) (string, error) {
		if id == "slow" {
			return "", errors.New("pane did not answer")
		}
		return "", nil
	}

	deliveries, settled, pending := nativeResumePlan(panes, probe, sessionAlwaysExists, nil)
	if pending != 1 {
		t.Errorf("the unprobeable pane must be pending, got %d", pending)
	}
	if len(deliveries) != 1 || deliveries[0].PaneID != "ready" {
		t.Errorf("the ready pane must still be resumed now, got %+v", deliveries)
	}
	for _, id := range settled {
		if id == "slow" {
			t.Error("a pane that never answered must not be settled")
		}
	}
}
