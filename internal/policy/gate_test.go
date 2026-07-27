package policy

import (
	"strings"
	"testing"
)

func TestGate_DefaultsToUnblocked(t *testing.T) {
	t.Cleanup(ClearBlocked)
	ClearBlocked()

	if reason, blocked := Blocked(); blocked {
		t.Fatalf("gate must default to unblocked, got blocked with reason %q", reason)
	}
}

func TestGate_SetAndClear(t *testing.T) {
	t.Cleanup(ClearBlocked)

	SetBlocked(".rysh/policy.yaml failed to parse: bad yaml")
	reason, blocked := Blocked()
	if !blocked {
		t.Fatal("expected blocked after SetBlocked")
	}
	if !strings.Contains(reason, "bad yaml") {
		t.Fatalf("reason should carry the parse error, got %q", reason)
	}

	ClearBlocked()
	if _, blocked := Blocked(); blocked {
		t.Fatal("expected unblocked after ClearBlocked")
	}
}

// TestGate_EmptyReasonStillBlocks guards against a caller accidentally
// disarming the gate by passing an empty reason: fail-closed means an empty
// explanation must not read as "not blocked".
func TestGate_EmptyReasonStillBlocks(t *testing.T) {
	t.Cleanup(ClearBlocked)

	SetBlocked("")
	reason, blocked := Blocked()
	if !blocked {
		t.Fatal("SetBlocked(\"\") must still block")
	}
	if reason == "" {
		t.Fatal("a default reason must be substituted so the user sees why")
	}
}

func TestBlockedMessage_TellsUserHowToRecover(t *testing.T) {
	got := BlockedMessage("policy.yaml failed to parse")
	for _, want := range []string{"BLOCKED", "##policy reload"} {
		if !strings.Contains(got, want) {
			t.Fatalf("refusal message must contain %q, got:\n%s", want, got)
		}
	}
}
