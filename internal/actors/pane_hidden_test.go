// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Hidden survives a daemon restart (design 027 §5.1).
//
// The board claude is hidden by DEFAULT, so a lost field does not degrade
// gracefully: it puts an agent's pane back on everyone's screen after every
// restart, and the human's fix is to hide it again each time.

func TestHiddenSurvivesTheSnapshotRoundTrip(t *testing.T) {
	p := &PaneActor{id: "pane-1", hidden: true}

	snap := p.buildSnapshot(false, false, false)
	if !snap.Hidden {
		t.Fatal("buildSnapshot dropped Hidden — the pane would come back visible")
	}

	restored := &PaneActor{id: "pane-1"}
	restored.RestoreState(snap)
	if !restored.hidden {
		t.Fatal("RestoreState dropped Hidden — a hidden pane reappears after a daemon restart")
	}
}

func TestAVisiblePaneStaysVisibleAcrossTheRoundTrip(t *testing.T) {
	p := &PaneActor{id: "pane-1"}

	snap := p.buildSnapshot(false, false, false)
	if snap.Hidden {
		t.Fatal("a pane nobody hid came back Hidden")
	}

	restored := &PaneActor{id: "pane-1", hidden: true}
	restored.RestoreState(snap)
	if restored.hidden {
		t.Fatal("RestoreState left a stale hidden=true rather than taking the snapshot's value")
	}
}

// A group keeps its own copy of `hidden` so it can cycle focus without a round
// trip per keystroke, and reconciles it from what the pane reports. The
// reconcile must distinguish "the pane says visible" from "the pane did not
// answer" — otherwise a busy daemon un-hides panes, which is the F-23 shape:
// a zero value from a broken path read as a fact.
func TestAnUnansweredSnapshotCannotUnhideAPane(t *testing.T) {
	ref := &paneRefInGroup{id: "board-agent", hidden: true}

	// Exactly what collectSnapshot puts in the slot when the round trip fails:
	// a placeholder with every field at its zero value, Hidden included.
	placeholder := domain.PaneSnapshot{ID: "board-agent", Status: "unreachable"}
	reconcileRefFromSnapshot(ref, placeholder, false)

	if !ref.hidden {
		t.Fatal("an unreachable pane un-hid itself — a busy daemon would put the board claude back on screen")
	}
}

func TestAnAnsweredSnapshotIsAuthoritative(t *testing.T) {
	ref := &paneRefInGroup{id: "board-agent", hidden: true}

	reconcileRefFromSnapshot(ref, domain.PaneSnapshot{ID: "board-agent"}, true)
	if ref.hidden {
		t.Fatal("the pane said it is visible and the group kept believing otherwise")
	}

	// And the direction that carries a daemon restart: the group is rebuilt
	// with no memory, the pane restores hidden from KV and says so.
	reconcileRefFromSnapshot(ref, domain.PaneSnapshot{ID: "board-agent", Hidden: true}, true)
	if !ref.hidden {
		t.Fatal("the group ignored a restored pane reporting itself hidden")
	}
}
