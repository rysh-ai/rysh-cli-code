package actors

import (
	"errors"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// TestPolicyBlocked_FailsClosedOnParseError is the regression guard for an
// inverted safety contract. policy.Load documents that a caller "must not treat
// the session as governed" when Err is set, but WorkspaceActor only logged the
// error and carried on with an empty policy — so a typo in .rysh/policy.yaml
// silently dropped proxy.required and every budget ceiling and ran the session
// ungoverned, the exact outcome design 013 §1 calls non-negotiable.
func TestPolicyBlocked_FailsClosedOnParseError(t *testing.T) {
	tests := []struct {
		name    string
		policy  *policy.Policy
		blocked bool
	}{
		{
			name:    "nil policy is not blocked (nothing loaded yet)",
			policy:  nil,
			blocked: false,
		},
		{
			name:    "empty policy is not blocked (no file is a valid posture)",
			policy:  policy.Empty(),
			blocked: false,
		},
		{
			name:    "unparseable policy blocks governed execution",
			policy:  &policy.Policy{Path: ".rysh/policy.yaml", Err: errors.New("parse policy.yaml: bad yaml")},
			blocked: true,
		},
		{
			name:    "successfully loaded policy is not blocked",
			policy:  &policy.Policy{Path: ".rysh/policy.yaml", Loaded: true},
			blocked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &WorkspaceActor{policy: tc.policy}
			if got := w.policyBlocked(); got != tc.blocked {
				t.Fatalf("policyBlocked() = %v, want %v", got, tc.blocked)
			}
		})
	}
}

// TestPolicyGateBlocked_HeadlessDispatchStillRefuses covers the fan-in helper
// used by AgentActor / HumanoidActor / the automation loops. Those dispatch
// sites are headless — there may be no pane to write to — so the helper must
// still refuse (and must not panic on a nil publisher or empty pane).
func TestPolicyGateBlocked_HeadlessDispatchStillRefuses(t *testing.T) {
	t.Cleanup(policy.ClearBlocked)

	policy.ClearBlocked()
	if policyGateBlocked(nil, "", "agent.test.prompt") {
		t.Fatal("must not refuse while the policy is clean")
	}

	policy.SetBlocked(".rysh/policy.yaml failed to parse: boom")
	if !policyGateBlocked(nil, "", "agent.test.prompt") {
		t.Fatal("must refuse headless dispatch while the policy is unparseable")
	}
	if !policyGateBlocked(nil, "", "agent.test.continue") {
		t.Fatal("must refuse resumption too — a continue carries no new prompt")
	}
}

// TestPolicyBlocked_ClearsAfterSuccessfulReload proves the escape hatch works:
// the session is not bricked by a bad policy — the operator fixes the file and
// ##policy reload restores governed execution.
func TestPolicyBlocked_ClearsAfterSuccessfulReload(t *testing.T) {
	w := &WorkspaceActor{policy: &policy.Policy{Err: errors.New("parse error")}}
	if !w.policyBlocked() {
		t.Fatal("expected blocked while the policy is unparseable")
	}

	// Simulate ##policy reload finding a valid file.
	w.policy = policy.Empty()
	if w.policyBlocked() {
		t.Fatal("expected governed execution to resume after a clean reload")
	}
}
