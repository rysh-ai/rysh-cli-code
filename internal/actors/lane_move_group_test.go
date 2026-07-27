package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newTestLane builds a LaneActor populated with n single-pane groups
// (ids "g0".."gn-1") and the given active group index, with no NATS/actor deps.
func newTestLane(n, active int) *LaneActor {
	l := &LaneActor{id: "lane-0000000000000000000000000000", activeGroup: active}
	for i := 0; i < n; i++ {
		l.groupRefs = append(l.groupRefs, &laneGroupRef{
			id:        groupIDForIndex(i),
			paneCount: 1,
		})
	}
	return l
}

func groupIDForIndex(i int) string {
	return string(rune('a'+i)) + "00000000000000000000000000000000"
}

// groupOrder returns the current group id order (first char each) for comparison.
func groupOrder(l *LaneActor) string {
	s := ""
	for _, r := range l.groupRefs {
		s += r.id[:1]
	}
	return s
}

func TestMoveActiveGroupUp(t *testing.T) {
	l := newTestLane(3, 2) // order a,b,c ; active = c (index 2)
	l.moveActiveGroup(msg.DirUp)
	if got, want := groupOrder(l), "acb"; got != want {
		t.Fatalf("order after up = %q, want %q", got, want)
	}
	if l.activeGroup != 1 {
		t.Fatalf("activeGroup = %d, want 1 (moved group stays active)", l.activeGroup)
	}
	if l.groupRefs[l.activeGroup].id[:1] != "c" {
		t.Fatalf("active group id = %q, want c", l.groupRefs[l.activeGroup].id[:1])
	}
}

func TestMoveActiveGroupUp_AtTopIsNoop(t *testing.T) {
	l := newTestLane(3, 0) // active already at top
	l.moveActiveGroup(msg.DirUp)
	if got, want := groupOrder(l), "abc"; got != want {
		t.Fatalf("order = %q, want unchanged %q", got, want)
	}
	if l.activeGroup != 0 {
		t.Fatalf("activeGroup = %d, want 0", l.activeGroup)
	}
}

func TestMoveActiveGroupDown(t *testing.T) {
	l := newTestLane(3, 0) // order a,b,c ; active = a (index 0)
	l.moveActiveGroup(msg.DirDown)
	if got, want := groupOrder(l), "bac"; got != want {
		t.Fatalf("order after down = %q, want %q", got, want)
	}
	if l.activeGroup != 1 {
		t.Fatalf("activeGroup = %d, want 1", l.activeGroup)
	}
}

func TestMoveActiveGroupDown_AtBottomIsNoop(t *testing.T) {
	l := newTestLane(3, 2) // active already at bottom
	l.moveActiveGroup(msg.DirDown)
	if got, want := groupOrder(l), "abc"; got != want {
		t.Fatalf("order = %q, want unchanged %q", got, want)
	}
	if l.activeGroup != 2 {
		t.Fatalf("activeGroup = %d, want 2", l.activeGroup)
	}
}

func TestMoveActiveGroup_SingleGroupIsNoop(t *testing.T) {
	l := newTestLane(1, 0)
	l.moveActiveGroup(msg.DirUp)
	l.moveActiveGroup(msg.DirDown)
	if got, want := groupOrder(l), "a"; got != want {
		t.Fatalf("order = %q, want unchanged %q", got, want)
	}
	if l.activeGroup != 0 {
		t.Fatalf("activeGroup = %d, want 0", l.activeGroup)
	}
}

func TestMoveActiveGroup_FullTraversal(t *testing.T) {
	l := newTestLane(3, 0) // a,b,c ; active a
	// Walk a down to the bottom: a,b,c -> b,a,c -> b,c,a
	l.moveActiveGroup(msg.DirDown)
	l.moveActiveGroup(msg.DirDown)
	if got, want := groupOrder(l), "bca"; got != want {
		t.Fatalf("order after two downs = %q, want %q", got, want)
	}
	if l.activeGroup != 2 || l.groupRefs[2].id[:1] != "a" {
		t.Fatalf("a not at bottom & active: activeGroup=%d order=%q", l.activeGroup, groupOrder(l))
	}
	// Walk a back to the top.
	l.moveActiveGroup(msg.DirUp)
	l.moveActiveGroup(msg.DirUp)
	if got, want := groupOrder(l), "abc"; got != want {
		t.Fatalf("order after two ups = %q, want %q", got, want)
	}
	if l.activeGroup != 0 || l.groupRefs[0].id[:1] != "a" {
		t.Fatalf("a not back at top & active: activeGroup=%d order=%q", l.activeGroup, groupOrder(l))
	}
}
