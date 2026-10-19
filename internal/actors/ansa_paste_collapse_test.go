// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"

	"github.com/nats-io/nats.go"
)

// ---------------------------------------------------------------------------
// F-44 — the trigger, isolated
// ---------------------------------------------------------------------------
//
// F-44: `##ansa prompt` reported `delivered`, exit 0, while a ~4k-character work
// order sat in the recipient's composer UNSUBMITTED FOR 12 MINUTES. The backlog
// row records the screen that was actually observed, and that detail is the
// whole diagnosis: the order was on screen "as three `[Pasted text #N]` chunks".
//
// It is NOT length alone, and the row says so — a comparably long dispatch
// landed. Nor is it ansaSubmitPause, which is why raising that constant would
// have been the wrong fix twice over: the verifier that exists to catch an
// absorbed CR is BLIND to this screen, so the pause is not the layer that failed.
//
// THE MECHANISM. Two independent things must coincide, which is exactly why it
// looked non-deterministic:
//
//  1. The CR is absorbed and the order stays in the composer. Already known,
//     already handled — ansaConfirmSubmitted (F-35/F-38) re-presses Enter for
//     precisely this.
//  2. The recipient's TUI COLLAPSES the delivered text into `[Pasted text #N]`
//     placeholders. ansaTypeIntoPane publishes the body as ONE MsgRawKeyInput,
//     the pane writes it straight to the PTY (pane.go, `p.ptyFile.Write`), and a
//     burst that large arrives at the reader in several chunks — each one
//     detected as a paste and rendered as a placeholder.
//
// Alone, (1) is recoverable and (2) is harmless. Together they are F-44:
// ansaComposerHolds looks for the first ansaSubmitProbeLen characters of the
// TEXT WE SENT, and after a paste collapse NONE of those characters are on the
// screen. The probe misses, "not held" is returned, ansaConfirmSubmitted
// concludes the order was submitted, and the re-press never fires — so the
// bounded retry that would have rescued the delivery declines to act at the one
// moment it was built for. Nothing errors. The receipt says `delivered`.
//
// That is the same shape as F-38, one layer further in: there the verifier
// looked in the wrong REGION of the screen, here it looks for the wrong TEXT.
//
// FIXED, red-then-green, in this same worktree: ansaComposerHolds now takes the
// whole order and falls back to the recipient's paste-placeholder vocabulary when
// the literal scan misses. The placeholder route was chosen over "the composer is
// non-empty" on blast radius — see the function's comment — and it is gated on
// ansaPasteCollapseMin so a short order can never claim a human's paste.
// ansaSubmitPause was NOT touched: the pause is not the layer that failed.

// paneWithPasteCollapsedOrder is F-44's observed screen: a 48-column fleet pane
// whose composer holds the work order as three paste placeholders, with the CR
// absorbed. Compare paneWithOrderInComposer in ansa_composer_test.go — same
// pane, same unsubmitted order, and NOT ONE CHARACTER of the order's text is
// rendered anywhere on it.
func paneWithPasteCollapsedOrder() []string {
	return []string{
		"  transcript line from an earlier turn",
		"✻ Baked for 6m 4s",
		"",
		"─────────────────────────────────────────────",
		"❯ [Pasted text #1 +47 lines] [Pasted text #2 +5",
		"  2 lines] [Pasted text #3 +38 lines]",
		"─────────────────────────────────────────────",
		"  ⏵⏵ bypass permissions on (shift+tab to     ·",
	}
}

// f44Order is a work order of F-44's size — the ~4k-character brief that was
// dispatched. Only its first ansaSubmitProbeLen characters ever reach the probe.
func f44Order() string {
	const head = "[FLEET agent-fleet | WORK ORDER | FROM roadmap-manager"
	body := head
	for len(body) < 4000 {
		body += " Read the order in full and act on it, in your own worktree."
	}
	return body
}

// CONDITION X — the trigger, and now the guard on the fix.
//
// The predicate is handed the WHOLE order, exactly as ansaConfirmSubmitted hands
// it over: the length is what tells the fix a paste collapse was possible at all,
// so a test that pre-cuts the probe cannot exercise the fix it is guarding.
func TestAPasteCollapsedOrderIsFoundInTheComposer(t *testing.T) {
	rows := paneWithPasteCollapsedOrder()
	order := f44Order()

	if !ansaComposerHolds(rows, order) {
		t.Errorf("F-44: an order sitting unsent in the composer as [Pasted text #N] was "+
			"reported as SUBMITTED, so the re-press never fires and the order never runs.\n"+
			"  probe (first %d chars of the order): %q\n"+
			"  composer row: %q",
			ansaSubmitProbeLen, order[:ansaSubmitProbeLen], rows[4])
	}
}

// The other half of the fix's contract, and the reason it is narrow: a SHORT
// text gets no placeholder benefit. A human sharing the pane types and pastes
// too, and a placeholder that counted regardless of what we sent would let the
// re-press submit a human's half-written line on their behalf.
func TestAShortOrderGetsNoPlaceholderBenefit(t *testing.T) {
	rows := paneWithPasteCollapsedOrder() // composer holds SOMEBODY's paste
	if ansaComposerHolds(rows, "ls -la") {
		t.Error("a [Pasted text #N] placeholder was credited to a six-character order — " +
			"the re-press would submit whatever a human had pasted or half-typed")
	}
}

// CONDITION Y — the control, and the half that makes this a reproduction rather
// than a theory. SAME code, SAME absorbed CR, SAME composer region: the only
// thing that changes is whether the recipient collapsed the text. Short enough
// to be echoed literally and the probe matches, so the re-press fires and the
// order lands. This one passes today and must keep passing after any fix.
func TestAnEchoedOrderIsStillFoundWhenNotCollapsed(t *testing.T) {
	const order = "Read /tmp/order-001.md in full and act on it"
	rows := paneWithOrderInComposer(order)
	if !ansaComposerHolds(rows, order) {
		t.Error("the control case broke: an order echoed literally must still be found, " +
			"otherwise the comparison in TestAPasteCollapsedOrderIsFoundInTheComposer " +
			"proves nothing about paste collapse")
	}
}

// The behavioural consequence at the receiving end, which is where E9 requires
// verification. The predicate above is the mechanism; this is the cost: before
// the fix, ZERO Enter re-presses reached the wire for a pane still holding the
// order. Note what it asserts — not that the code errors, but that it stayed
// SILENT, which is what made F-44 cost twelve minutes instead of failing loudly.
func TestConfirmSubmittedRePressesForAPasteCollapsedOrder(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const pane = "pane-f44-paste-collapsed"
	raw := make(chan []byte, 32)
	sub, err := nc.Subscribe(msg.T("pane", pane, "rawinput"), func(m *nats.Msg) { raw <- m.Data })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	// The pane never submits: every look shows the same collapsed composer.
	tr := &fakeAnsaTransport{screens: [][]string{paneWithPasteCollapsedOrder()}}

	prev := ansaSubmitRecheck
	ansaSubmitRecheck = time.Millisecond
	defer func() { ansaSubmitRecheck = prev }()

	if err := ansaConfirmSubmitted(pub, tr.Screen, pane, f44Order(), "claude"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	select {
	case <-raw: // a re-press reached the wire, which is the whole point
	case <-time.After(2 * time.Second):
		t.Error("F-44: ansaConfirmSubmitted sent NO re-press for a composer still holding " +
			"the order as [Pasted text #N] — it returned nil, the caller reported `delivered`, " +
			"and the order sat unsubmitted. Verification declined at exactly the moment it " +
			"was built for.")
	}
}
