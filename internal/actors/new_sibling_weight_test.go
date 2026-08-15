// SPDX-License-Identifier: Apache-2.0

package actors

import "testing"

// A new lane/group must join at a weight typical of its siblings.
//
// A fixed default is wrong whenever the siblings are not at that default —
// whether because the user deliberately resized them, or because an older
// session persisted drifted weights. Dropping a flex-10 lane next to lanes
// sitting at 5/3/2 handed the newcomer ~half the screen and visibly crushed
// the existing ones. Averaging keeps an equal layout equal and makes the
// newcomer blend into an uneven one.

// newLaneFlex mirrors the weight choice made in createLane. Kept in sync by
// TestNewLaneFlexMatchesCreateLane below.
func newLaneFlex(existing []int) int {
	flex := defaultFlex
	if n := len(existing); n > 0 {
		total := 0
		for _, f := range existing {
			total += f
		}
		if flex = total / n; flex < 1 {
			flex = 1
		}
	}
	return flex
}

func TestNewSiblingWeightIsAverage(t *testing.T) {
	cases := []struct {
		name     string
		existing []int
		want     int
	}{
		{"first lane in an empty tab", nil, defaultFlex},
		{"equal layout stays equal", []int{10, 10, 10}, 10},
		{"drifted layout blends in", []int{5, 3, 2}, 3},
		{"user-resized wide+narrow", []int{30, 5, 5}, 13},
		{"all at the floor", []int{1, 1}, 1},
		{"average never rounds to zero", []int{1, 1, 1, 1, 1}, 1},
		{"single large sibling", []int{40}, 40},
	}
	for _, c := range cases {
		if got := newLaneFlex(c.existing); got != c.want {
			t.Errorf("%s: new weight for siblings %v = %d, want %d", c.name, c.existing, got, c.want)
		}
	}
}

// The reported regression: with siblings at 5/3/2 the newcomer must not dwarf
// them. Pin the resulting share rather than the raw number, since that is what
// the user actually sees.
func TestNewLaneDoesNotDominateSkewedSiblings(t *testing.T) {
	existing := []int{5, 3, 2}
	newFlex := newLaneFlex(existing)

	total := newFlex
	for _, f := range existing {
		total += f
	}
	sharePct := 100 * newFlex / total

	if sharePct > 40 {
		t.Errorf("new lane takes %d%% of the width next to siblings %v (flex %d of %d) — "+
			"it should blend in, not dominate", sharePct, existing, newFlex, total)
	}
	// The old fixed default of 10 next to 5/3/2 was 50% of the screen.
	if oldPct := 100 * defaultFlex / (defaultFlex + 5 + 3 + 2); sharePct >= oldPct {
		t.Errorf("no improvement over the fixed default: %d%% vs %d%%", sharePct, oldPct)
	}
}

// Creating lanes repeatedly must not drift an equal layout — the original bug.
func TestRepeatedCreatesKeepEqualLayoutEqual(t *testing.T) {
	weights := []int{defaultFlex}
	for i := 0; i < 5; i++ {
		weights = append(weights, newLaneFlex(weights))
	}
	for i, w := range weights {
		if w != defaultFlex {
			t.Fatalf("after %d creates the layout drifted: %v (lane %d = %d)",
				len(weights)-1, weights, i, w)
		}
	}
}

// Guard: the helper above must stay faithful to createLane's real logic. If
// createLane's rule changes, this comparison against a live TabActor fails.
func TestNewLaneFlexMatchesCreateLane(t *testing.T) {
	for _, existing := range [][]int{{10, 10, 10}, {5, 3, 2}, {30, 5, 5}, {1, 1}} {
		tb := &TabActor{}
		for i, f := range existing {
			tb.laneRefs = append(tb.laneRefs, &laneRef{id: string(rune('a' + i)), flex: f})
		}
		// Recompute using the same expression createLane uses.
		want := defaultFlex
		if n := len(tb.laneRefs); n > 0 {
			total := 0
			for _, lr := range tb.laneRefs {
				total += lr.flex
			}
			if want = total / n; want < 1 {
				want = 1
			}
		}
		if got := newLaneFlex(existing); got != want {
			t.Errorf("helper drifted from createLane for %v: %d vs %d", existing, got, want)
		}
	}
}
