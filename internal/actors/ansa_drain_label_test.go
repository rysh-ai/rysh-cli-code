// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"strings"
	"testing"
)

// F-32 (reopened 2026-08-11, recommendation (a), approved under the founder's
// autonomy policy rr-order-129): a bounded screen-watch cannot prove a queue is
// EMPTY, only that nothing was happening when it looked. So every label that
// rests on a quiet screen read says so (a trailing *), and the fleet receipt
// explains the star and names the verb that does verify: `##ansa kill`
// (F-42, process state).

func TestF32QuietLabelsAreMarkedNotGuaranteed(t *testing.T) {
	for _, tc := range []struct {
		d    ansaDrainOutcome
		want string
	}{
		{ansaDrainOutcome{Quiet: true}, "stopped*"},
		{ansaDrainOutcome{Quiet: true, Rounds: 2}, "drained+2*"},
		{ansaDrainOutcome{Quiet: true, Rewoke: 1, Rounds: 1}, "rewoke+1*"},
		{ansaDrainOutcome{Quiet: false}, "WORKING"}, // not a stop claim: unchanged
	} {
		if got := ansaDrainLabel(tc.d); got != tc.want {
			t.Errorf("ansaDrainLabel(%+v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestF32TheStarIsExplainedAndPointsAtTheVerifiedStop(t *testing.T) {
	for _, want := range []string{"*", "not proof", "queued", "F-32", "##ansa kill"} {
		if !strings.Contains(ansaDrainLabelNote, want) {
			t.Errorf("the label note does not say %q:\n%s", want, ansaDrainLabelNote)
		}
	}
	if !strings.HasSuffix(ansaDrainLabelNote, "\n") {
		t.Error("the note must end in a newline")
	}
}

// Every door that prints drain labels also prints the note — a source test,
// for the reason TestEveryInterruptReceiptStatesTheQueueLimit gives: a new door
// that forgets it would pass any behavioural test of the old one.
func TestF32EveryDoorWithDrainLabelsPrintsTheNote(t *testing.T) {
	src, err := os.ReadFile("workspace_ansa.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	labels := strings.Count(s, "ansaDrainLabel(o.")
	notes := strings.Count(s, "out.WriteString(ansaDrainLabelNote)")
	if labels == 0 {
		t.Fatal("no drain-label door found — this test guards nothing")
	}
	if notes != labels {
		t.Errorf("%d door(s) print drain labels but %d print the F-32 note", labels, notes)
	}
}
