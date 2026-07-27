package actors

import (
	"strings"
	"testing"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// clearPolicyGlobals resets the process-global fail-closed gate and approval
// policy between tests so state does not leak.
func clearPolicyGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		policy.ClearBlocked()
		sharedagentic.SetApprovalPolicy(nil)
	})
}

// TestSNATRequired_BlocksUntilRegistered is the enforcement proof for design 013
// snat.required: a required secret that is not registered for redaction must
// block governed execution (fail-closed), and the block must clear the moment
// the secret is registered — without a ##policy reload.
func TestSNATRequired_BlocksUntilRegistered(t *testing.T) {
	clearPolicyGlobals(t)

	// STRIPE_KEY is registered; GITHUB_TOKEN is not. Policy requires both.
	w := newSnatTestWorkspace(t, map[string]map[string]string{
		"ws-main": {"STRIPE_KEY": "sk_live_4eC39HqLyjWDarjtT1zdp7dc"},
	})
	w.policy = &policy.Policy{SNATRequired: []string{"STRIPE_KEY", "GITHUB_TOKEN"}, Loaded: true}

	// missingRequiredSecrets reports exactly the unregistered one.
	if got := w.missingRequiredSecrets(); len(got) != 1 || got[0] != "GITHUB_TOKEN" {
		t.Fatalf("missingRequiredSecrets = %v, want [GITHUB_TOKEN]", got)
	}

	// syncPolicyGate blocks, naming the missing secret.
	w.syncPolicyGate()
	reason, blocked := policy.Blocked()
	if !blocked {
		t.Fatal("governed execution not blocked while a required secret is missing")
	}
	if !strings.Contains(reason, "GITHUB_TOKEN") {
		t.Fatalf("block reason should name the missing secret, got: %q", reason)
	}

	// Register the missing secret and re-sync via the ##secret mutation hook —
	// the block must clear without a ##policy reload.
	wsScope, _ := w.secretWorkspaceScope()
	// persist=true → the project-local file store (the session KV store needs a
	// live JetStream, absent in this unit test).
	if err := w.secrets.Set(wsScope, "GITHUB_TOKEN", "ghp_016C7869012345678901234567890123456", true); err != nil {
		t.Fatalf("register secret: %v", err)
	}
	w.afterSecretMutation()

	if _, blocked := policy.Blocked(); blocked {
		t.Fatal("block did not clear after the required secret was registered")
	}
	if got := w.missingRequiredSecrets(); len(got) != 0 {
		t.Fatalf("missingRequiredSecrets after register = %v, want none", got)
	}
}

// TestSNATRequired_NoneRequired confirms the common case is unaffected: with no
// snat.required list, nothing is missing and nothing is blocked.
func TestSNATRequired_NoneRequired(t *testing.T) {
	clearPolicyGlobals(t)
	w := newSnatTestWorkspace(t, map[string]map[string]string{"ws-main": {"STRIPE_KEY": "sk_live_4eC39HqLyjWDarjtT1zdp7dc"}})
	w.policy = &policy.Policy{Loaded: true} // no SNATRequired

	if got := w.missingRequiredSecrets(); got != nil {
		t.Fatalf("missingRequiredSecrets with no requirement = %v, want nil", got)
	}
	w.syncPolicyGate()
	if _, blocked := policy.Blocked(); blocked {
		t.Fatal("blocked with no snat.required list")
	}
}

// TestRenderRequiredSecretStatus checks the ##policy annotation reflects the
// missing vs registered state.
func TestRenderRequiredSecretStatus(t *testing.T) {
	clearPolicyGlobals(t)
	w := newSnatTestWorkspace(t, map[string]map[string]string{"ws-main": {"STRIPE_KEY": "sk_live_4eC39HqLyjWDarjtT1zdp7dc"}})
	w.policy = &policy.Policy{SNATRequired: []string{"STRIPE_KEY", "GITHUB_TOKEN"}, Loaded: true}

	var out strings.Builder
	w.renderRequiredSecretStatus(&out)
	if !strings.Contains(out.String(), "BLOCKED") || !strings.Contains(out.String(), "GITHUB_TOKEN") {
		t.Fatalf("status should flag the missing secret, got: %q", out.String())
	}
}
