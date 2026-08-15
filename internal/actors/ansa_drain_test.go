// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"testing"
	"time"
)

// F-30, second half: founder decision 2026-08-10 — "stop all fleet" CLEARS the
// queue, accepting that queued orders are cancelled rather than deferred.
//
// The first half (a receipt that stated the limit) shipped earlier the same day.
// This is the behaviour it was standing in for: ESC ends the current turn, the
// target's queue then starts the next order on its own, and a stop that does not
// keep going leaves the fleet working seconds after it reported a stop.

func withFastDrain(t *testing.T) {
	t.Helper()
	prev := ansaDrainSettle
	ansaDrainSettle = time.Millisecond
	t.Cleanup(func() { ansaDrainSettle = prev })
}

var (
	busyFrame   = []string{"⏺ writing…", "  ⏵⏵ bypass permissions on · esc to interrupt · ← for agents"}
	queuedFrame = []string{"❯ Press up to edit queued messages"}
	quietFrame  = []string{"❯ ", "  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents"}
)

// A pane that is already quiet costs one look and NO extra keystrokes. Sending
// ESC to an idle agent is not free: it dismisses whatever it is showing.
func TestDrainingAQuietPaneSendsNothingFurther(t *testing.T) {
	withFastDrain(t)
	tr := &fakeAnsaTransport{screens: [][]string{quietFrame}}
	got := ansaDrainPane(tr, "pane-a")
	if !got.Quiet || got.Rounds != 0 {
		t.Errorf("quiet pane: got quiet=%v rounds=%d, want true/0", got.Quiet, got.Rounds)
	}
	if len(tr.interrupted) != 0 {
		t.Errorf("an idle pane was interrupted anyway: %v", tr.interrupted)
	}
}

// The case the whole change exists for: the queue starts the next order, and the
// drain cancels that too.
func TestDrainingCancelsTheOrderTheQueueStarted(t *testing.T) {
	withFastDrain(t)
	tr := &fakeAnsaTransport{screens: [][]string{busyFrame, quietFrame}}
	got := ansaDrainPane(tr, "pane-a")
	if !got.Quiet {
		t.Fatal("the pane never went quiet")
	}
	// TWO, not one, and deliberately: work is now "the screen moved", and the
	// move INTO stillness is itself a move. So the frame where a pane stops
	// costs one extra ESC. Accepted — it lands on an idle claude and clears its
	// composer, which after F-31 is useful rather than harmful, and the
	// alternative is a cleverer rule that has to decide what a change MEANS.
	if got.Rounds != 2 {
		t.Errorf("rounds = %d, want 2 (the cancel, plus the stop transition)", got.Rounds)
	}
	if len(tr.interrupted) != 2 {
		t.Errorf("wire says %d interrupts, want 2: %v", len(tr.interrupted), tr.interrupted)
	}
}

// "Press up to edit queued messages" is a queue with no turn running yet. It
// must count as work, or the drain returns the instant a turn ends and the queue
// starts the next order behind its back.
func TestAQueueWithNoRunningTurnStillCountsAsWork(t *testing.T) {
	withFastDrain(t)
	tr := &fakeAnsaTransport{screens: [][]string{queuedFrame, quietFrame}}
	// >=1: the queue frame earns an interrupt, and the transition into
	// stillness earns the second (see TestDrainingCancelsTheOrderTheQueueStarted).
	if got := ansaDrainPane(tr, "pane-a"); got.Rounds < 1 {
		t.Errorf("a visible queue was treated as quiet: rounds=%d", got.Rounds)
	}
}

// An agent that keeps producing work must not be chased forever — the loop is
// bounded and says so rather than spinning.
func TestDrainingIsBoundedAndReportsStillWorking(t *testing.T) {
	withFastDrain(t)
	tr := &fakeAnsaTransport{screens: [][]string{busyFrame}} // never goes quiet
	got := ansaDrainPane(tr, "pane-a")
	if got.Quiet {
		t.Error("a pane that never stopped was reported quiet")
	}
	if got.Rounds != ansaDrainRounds {
		t.Errorf("rounds = %d, want the bound %d", got.Rounds, ansaDrainRounds)
	}
	if ansaDrainLabel(got) != "WORKING" {
		t.Errorf("label = %q, want WORKING", ansaDrainLabel(got))
	}
}

// A pane that cannot be read is UNKNOWN, never quiet. Collapsing the two is the
// receipt-without-delivery this track keeps re-finding: it would report a stop
// for a pane nobody could see.
func TestAnUnreadablePaneIsNotReportedStopped(t *testing.T) {
	withFastDrain(t)
	tr := &fakeAnsaTransport{screenErr: errors.New("nats: timeout")}
	got := ansaDrainPane(tr, "pane-a")
	if got.Quiet {
		t.Error("a pane whose screen could not be read was reported quiet")
	}
	if got.Unreadable == nil {
		t.Error("the read failure was swallowed")
	}
	if l := ansaDrainLabel(got); l != "unread " {
		t.Errorf("label = %q, want unread", l)
	}
}

// THE NARROW-PANE TRAP, pinned. At 49 columns claude's footer renders
// "esc to interr…". A full-string match reports that busy pane as idle — which
// is exactly what made F-29 look like a defect for a day, and here it would make
// the drain stop early and leave a fleet running.
func TestATruncatedFooterStillReadsAsWorking(t *testing.T) {
	for _, row := range []string{
		"  ⏵⏵ bypass permissions on (shift+tab to      · esc to interr…",
		"esc to interrupt",
		"· esc to i…",
	} {
		if !ansaScreenIsWorking([]string{row}) {
			t.Errorf("a working pane read as quiet: %q", row)
		}
	}
	if ansaScreenIsWorking([]string{quietFrame[0], quietFrame[1]}) {
		t.Error("an idle pane read as working")
	}
}

// The fleet-wide stop must not chase a pane whose FIRST interrupt failed: there
// is nothing to drain, and re-reading it four times delays every other pane's
// verification for no gain.
func TestAFailedFirstInterruptIsNotDrained(t *testing.T) {
	withFastDrain(t)
	tr := &fakeAnsaTransport{
		panes: []ansaPane{
			{ID: "pane-a", GivenName: "wkr-a", Meta: map[string]string{"fleet.role": "worker"}},
		},
		interruptErr: errors.New("nats: connection closed"),
		screens:      [][]string{busyFrame},
	}
	r := &ansaRouter{tr: tr}
	out, refusal := r.InterruptFleet("")
	if refusal != nil {
		t.Fatalf("unexpected refusal: %s", refusal.Error)
	}
	if out[0].Drain.Rounds != 0 || out[0].Drain.Quiet {
		t.Errorf("a pane the first interrupt never reached was drained anyway: %+v", out[0].Drain)
	}
}

// ---------------------------------------------------------------------------
// F-32 — one quiet reading is not evidence a queue is empty
// ---------------------------------------------------------------------------

// THE DEFECT, pinned. The pane is idle when first looked at — the gap between a
// cancelled turn and its queue starting the next order — then working again. A
// drain that returns on the first quiet read calls that "stopped" and leaves the
// fleet running, which is F-30 with a receipt.
func TestOneQuietReadingIsNotAStop(t *testing.T) {
	withFastDrain(t)
	tr := &fakeAnsaTransport{screens: [][]string{
		quietFrame,                         // the handover gap — idle, but the queue is about to fire
		busyFrame,                          // the queued order started
		quietFrame, quietFrame, quietFrame, // genuinely done
	}}
	got := ansaDrainPane(tr, "pane-a")
	if !got.Quiet {
		t.Fatal("the pane never settled")
	}
	if got.Rounds < 1 {
		t.Errorf("rounds = %d — the order the queue started was never cancelled", got.Rounds)
	}
	if len(tr.interrupted) < 1 {
		t.Error("the quiet-then-busy pane was left running")
	}
}

// A pane that is quiet throughout still has to be LOOKED AT more than once
// before it is called stopped, or the check is the same single sample that
// produced F-32.
func TestAStopRequiresConsecutiveQuietLooks(t *testing.T) {
	withFastDrain(t)
	tr := &fakeAnsaTransport{screens: [][]string{quietFrame}}
	got := ansaDrainPane(tr, "pane-a")
	if !got.Quiet || got.Rounds != 0 {
		t.Fatalf("quiet pane: quiet=%v rounds=%d, want true/0", got.Quiet, got.Rounds)
	}
	if tr.screenCall < ansaDrainQuietLooks {
		t.Errorf("screen read %d time(s), want at least %d consecutive quiet looks",
			tr.screenCall, ansaDrainQuietLooks)
	}
	if len(tr.interrupted) != 0 {
		t.Errorf("an idle pane was interrupted: %v", tr.interrupted)
	}
}

// Work RESETS the streak. Quiet, quiet, busy must not be two-thirds of the way
// to "stopped" — it must start counting again after the interrupt.
func TestWorkResetsTheQuietStreak(t *testing.T) {
	withFastDrain(t)
	tr := &fakeAnsaTransport{screens: [][]string{
		quietFrame, quietFrame, // almost declared stopped…
		busyFrame,                          // …but the queue fired
		quietFrame, quietFrame, quietFrame, // now it is really done
	}}
	got := ansaDrainPane(tr, "pane-a")
	if !got.Quiet {
		t.Fatal("never settled")
	}
	if got.Rounds < 1 {
		t.Errorf("rounds = %d — the queued order was never cancelled", got.Rounds)
	}
}

// ---------------------------------------------------------------------------
// F-32, real cause: the marker is not rendered at fleet pane widths
// ---------------------------------------------------------------------------

// A fleet pane in a four-lane layout is 48 COLUMNS. At that width claude's
// footer renders "⏵⏵ bypass permissions on (shift+tab to     ·" — "esc to
// interrupt" is not truncated, it is absent. Measured on three live panes: the
// worker was generating and no marker existed anywhere on its screen.
//
// So a drain that only matches text calls a working pane stopped, which is what
// F-32 was. The screen still CHANGES while something writes to it, and that
// signal does not depend on width.
var narrowBusyA = []string{
	"protocol that is safe in all executions and",
	"live in those that stabilise. This is the",
	"  ⏵⏵ bypass permissions on (shift+tab to     ·",
}
var narrowBusyB = []string{
	"stance of Paxos, Raft, Viewstamped Replication",
	"and ZAB, and it is the right engineering",
	"  ⏵⏵ bypass permissions on (shift+tab to     ·",
}

func TestANarrowPaneThatIsWritingCountsAsWorking(t *testing.T) {
	withFastDrain(t)
	// No marker on any frame; only the CONTENT moves. This is a real capture
	// shape from a 48-column fleet pane.
	tr := &fakeAnsaTransport{screens: [][]string{
		narrowBusyA, narrowBusyB, // writing
		narrowBusyB, narrowBusyB, narrowBusyB, // stopped: frames identical
	}}
	got := ansaDrainPane(tr, "pane-narrow")
	if got.Rounds == 0 {
		t.Error("a 48-column pane that was visibly writing was reported stopped without a single " +
			"extra interrupt — this is F-32, and no marker exists at that width")
	}
	if !got.Quiet {
		t.Error("the pane went still and was never called stopped")
	}
}

// The other half: identical frames with no marker must NOT be read as working,
// or every idle pane is interrupted four times over.
func TestAStillNarrowPaneIsNotMistakenForWork(t *testing.T) {
	withFastDrain(t)
	tr := &fakeAnsaTransport{screens: [][]string{narrowBusyB}} // same frame forever
	got := ansaDrainPane(tr, "pane-idle")
	if !got.Quiet || got.Rounds != 0 {
		t.Errorf("idle narrow pane: quiet=%v rounds=%d, want true/0", got.Quiet, got.Rounds)
	}
}

// ---------------------------------------------------------------------------
// F-41 — a stopped pane wakes itself, and the re-sweep puts it back down
// ---------------------------------------------------------------------------

func withFastResweep(t *testing.T) {
	t.Helper()
	pr, ps := ansaFleetResweeps, ansaResweepSettle
	ansaFleetResweeps, ansaResweepSettle = 3, time.Millisecond
	t.Cleanup(func() { ansaFleetResweeps, ansaResweepSettle = pr, ps })
}

// THE PROVEN CASE, scripted: the pane goes down clean, then wakes — a
// background task completed and its task-notification started a new turn with
// no keystroke (transcript evidence, 2026-08-11: ESC 13:45:27Z, notification
// 13:46:12Z, fresh essay after the stop). The re-sweep must see the wake and
// send another ESC; without phase 3 the receipt says stopped and the pane
// works on.
func TestAPaneThatWakesItselfIsPutBackDown(t *testing.T) {
	withFastDrain(t)
	withFastResweep(t)
	tr := &fakeAnsaTransport{
		panes: []ansaPane{{ID: "pane-bg", GivenName: "wkr", Meta: map[string]string{"fleet.role": "worker"}}},
		screens: [][]string{
			// phase-2 drain: down clean
			quietFrame, quietFrame, quietFrame,
			// resweep baseline
			quietFrame,
			// next resweep look: the background task woke it (marker-less
			// narrow pane; only the changed screen betrays it — F-32's rule)
			narrowBusyA,
			// put down again; inner drain + re-baseline read
			quietFrame, quietFrame, quietFrame, quietFrame,
		},
	}
	r := &ansaRouter{tr: tr}
	out, refusal := r.InterruptFleet("")
	if refusal != nil {
		t.Fatalf("refusal: %s", refusal.Error)
	}
	if got := out[0].Drain.Rewoke; got != 1 {
		t.Errorf("Rewoke = %d, want 1 — the self-woken pane was not seen", got)
	}
	if len(tr.interrupted) < 2 {
		t.Errorf("wire shows %d interrupts, want ≥2: the wake never got its ESC — "+
			"the pane keeps working while the receipt says stopped", len(tr.interrupted))
	}
	if !out[0].Drain.Quiet {
		t.Error("the pane was never put back down")
	}
	if l := ansaDrainLabel(out[0].Drain); l != "rewoke+1" {
		t.Errorf("label = %q, want rewoke+1 — the wake is the headline, because the "+
			"operator must expect it to wake AGAIN", l)
	}
}

// The other half: a pane that stays down costs exactly one ESC. The re-sweep
// must not manufacture wakes out of an idle pane's static screen.
func TestAPaneThatStaysDownIsNotReInterrupted(t *testing.T) {
	withFastDrain(t)
	withFastResweep(t)
	tr := &fakeAnsaTransport{
		panes:   []ansaPane{{ID: "pane-calm", GivenName: "wkr", Meta: map[string]string{"fleet.role": "worker"}}},
		screens: [][]string{quietFrame},
	}
	r := &ansaRouter{tr: tr}
	out, refusal := r.InterruptFleet("")
	if refusal != nil {
		t.Fatalf("refusal: %s", refusal.Error)
	}
	if len(tr.interrupted) != 1 {
		t.Errorf("wire shows %d interrupts, want exactly 1 (the sweep): %v", len(tr.interrupted), tr.interrupted)
	}
	if out[0].Drain.Rewoke != 0 {
		t.Errorf("Rewoke = %d on a pane that never woke", out[0].Drain.Rewoke)
	}
	if l := ansaDrainLabel(out[0].Drain); l != "stopped" {
		t.Errorf("label = %q, want stopped", l)
	}
}
