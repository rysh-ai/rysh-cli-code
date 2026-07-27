package policy

import "sync/atomic"

// The fail-closed gate.
//
// Design 013 §1 requires that an unparseable policy file block governed
// execution: an unknown posture is not the same as "no rules". The problem is
// reach — the policy is loaded by WorkspaceActor, but agentic prompts are
// dispatched from PaneActor, AgentRegistryActor and HumanoidActor, none of
// which can see WorkspaceActor state, and two of the entry points are
// network-facing (remote `exec_prompt` over a share, and inbound humanoid
// channel messages) so they never pass through the workspace at all.
//
// A process-global mirrors how the governance proxy publishes its endpoint
// (internal/proxy/endpoint.go) and gives every dispatch site a cheap,
// lock-free read. It is deliberately process-wide rather than per-workspace:
// in farm mode that makes the most restrictive policy win, which is the
// correct direction to err.
//
// Set by WorkspaceActor on policy load and on ##policy reload.
var blockedReason atomic.Pointer[string]

// SetBlocked marks governed execution as refused, with a human-readable reason
// that dispatch sites surface to the user.
func SetBlocked(reason string) {
	if reason == "" {
		reason = "policy file failed to parse"
	}
	blockedReason.Store(&reason)
}

// ClearBlocked lifts the block after a policy file parses cleanly.
func ClearBlocked() { blockedReason.Store(nil) }

// Blocked reports whether governed execution must be refused, and why.
//
// Callers must fail closed: refuse the prompt and tell the user. Shell
// execution and ## commands are intentionally NOT gated on this, so the
// operator can repair the file and run ##policy reload.
func Blocked() (string, bool) {
	p := blockedReason.Load()
	if p == nil || *p == "" {
		return "", false
	}
	return *p, true
}

// BlockedMessage renders the standard refusal shown in a pane. The reason is
// cause-specific (a parse failure or a missing snat.required secret); the
// footer stays generic so it reads correctly for either.
func BlockedMessage(reason string) string {
	return "policy: BLOCKED — " + reason + "\n" +
		"  Agent and prompt execution are refused (fail-closed, design 013).\n" +
		"  Resolve the issue above, then run: ##policy reload\n"
}
