// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"strings"
	"testing"
)

// F-30: `stop all fleet` stops the TURN, and the fleet is working again seconds
// later.
//
// Measured 2026-08-10 on an isolated session: a busy pane with one message
// queued behind it was interrupted, and the queued message ran immediately —
// the turn was cancelled and the queue drained into a new one. The same pane
// with an empty queue stopped and stayed stopped. ESC is doing exactly what it
// says; the gap is between it and the operator's sentence.
//
// The fix this process can honestly make is to SAY so on every interrupt
// receipt (see ansaInterruptQueueCaution for why emptying the queue is a
// founder decision and why reporting queue depth is not available from here).
//
// These tests read SOURCE, deliberately, and it is the same argument
// TestEveryBusNewPassesASessionName makes for F-23: the rule is "every place
// that reports an interrupt also states the limit". That is a property of the
// CALL SITES, so a behavioural test of one path passes while a newly added
// third path silently omits it — which is precisely how this class of defect
// arrives.

func TestTheInterruptCautionSaysTheThreeThingsThatMatter(t *testing.T) {
	// The contract CHANGED on 2026-08-10 when the founder chose to clear the
	// queue, and this list changed with it. It used to require "CURRENT turn" —
	// the admission that ESC stops one turn and the fleet resumes. The stop now
	// drains, so the thing an operator must be told is no longer a limitation
	// but a COST: orders the fleet had already accepted are gone.
	for _, want := range []string{
		"CANCELLED", // the cost, stated in the word that cannot be skimmed past
		"queued",    // what is cancelled
		"F-30",      // the trail back to the evidence
	} {
		if !strings.Contains(ansaInterruptQueueCaution, want) {
			t.Errorf("the interrupt caution no longer mentions %q:\n%s",
				want, ansaInterruptQueueCaution)
		}
	}
	if !strings.HasSuffix(ansaInterruptQueueCaution, "\n") {
		t.Error("the caution must end in a newline, or it runs into the next line of the receipt")
	}
}

func TestEveryInterruptReceiptStatesTheQueueLimit(t *testing.T) {
	const file = "workspace_ansa.go"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	s := string(src)

	// One receipt line per interrupt door: single-target and --fleet.
	receipts := strings.Count(s, `"interrupt sent to`)
	cautions := strings.Count(s, "ansaInterruptQueueCaution")
	if receipts == 0 {
		t.Fatalf("no interrupt receipt found in %s — this test is guarding nothing", file)
	}
	if cautions != receipts {
		t.Errorf("%s reports an interrupt %d time(s) but states the queue limit %d time(s); "+
			"every door that says an interrupt was sent must also say what ESC does not do (F-30)",
			file, receipts, cautions)
	}
}
