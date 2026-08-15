// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-31: a delivery must never append to what was already on the composer line.
//
// Found on camera 2026-08-11, and it was a regression from F-30's own fix. The
// drained stop cancels a queued order; claude restores that order's text to the
// composer; the next work order was then typed onto the same line and arrived
// fused to it with no separator:
//
//	Reply with exactly the word QUEUED and nothing else.[FLEET demo | WORK ORDER …
//
// The agent obeyed the prefix, answered it, and never opened the order. Worse
// than a lost message because it ANSWERS — a `collect` waits, gets a prompt and
// plausible reply, and concludes the work was done.

// The whole fix in one assertion: two deliveries in a row must produce two
// independent messages, not a concatenation. This is the shape the defect had.
func TestASecondDeliveryDoesNotFuseWithTheFirst(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const pane = "pane-uuid-dirty-composer"
	raw := make(chan []byte, 16)
	sub, err := nc.Subscribe(msg.T("pane", pane, "rawinput"), func(m *nats.Msg) { raw <- m.Data })
	if err != nil {
		t.Fatalf("subscribe rawinput: %v", err)
	}
	defer sub.Unsubscribe()

	for _, text := range []string{"first order", "second order"} {
		if err := ansaDeliverToInbox(pub, pane, msg.AnsaModePrompt, text, "claude"); err != nil {
			t.Fatalf("Deliver(%q): %v", text, err)
		}
	}

	// clear, text, CR — twice.
	var got [][]byte
	for len(got) < 2*(ansaClearRepeats+2) {
		select {
		case d := <-raw:
			got = append(got, d)
		case <-time.After(4 * time.Second):
			t.Fatalf("want %d rawinput messages (N clears/text/CR twice), got %d", 2*(ansaClearRepeats+2), len(got))
		}
	}

	keys := make([]string, 0, len(got))
	for _, g := range got {
		_, m := ansaDecode(t, codecs, g)
		k, ok := m.(*msg.MsgRawKeyInput)
		if !ok {
			t.Fatalf("message is %T, want *msg.MsgRawKeyInput", m)
		}
		keys = append(keys, string(k.Data))
	}

	// The clear is REPEATED (ansaClearRepeats) because ctrl+u discards one line
	// and a 48-column composer wraps every work order. Assert the SHAPE:
	// N clears, the text, a CR — twice, in that order.
	clear := string(ansaClearComposerKey)
	want := []string{}
	for _, text := range []string{"first order", "second order"} {
		for i := 0; i < ansaClearRepeats; i++ {
			want = append(want, clear)
		}
		want = append(want, text, "\r")
	}
	if len(keys) != len(want) {
		t.Fatalf("wire has %d keys, want %d: %q", len(keys), len(want), keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("wire[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

// ORDER IS THE FIX. Clearing after the text would erase the message instead of
// the residue, which is a worse defect than the one being repaired — and a test
// that only counts three messages would pass on it.
func TestTheComposerIsClearedBEFORETheTextNotAfter(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const pane = "pane-uuid-order-matters"
	raw := make(chan []byte, 8)
	sub, err := nc.Subscribe(msg.T("pane", pane, "rawinput"), func(m *nats.Msg) { raw <- m.Data })
	if err != nil {
		t.Fatalf("subscribe rawinput: %v", err)
	}
	defer sub.Unsubscribe()

	if err := ansaDeliverToInbox(pub, pane, msg.AnsaModePrompt, "the work order", "claude"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	// Every clear must land BEFORE the text. Clearing after would erase the
	// message instead of the residue — a worse defect than the one repaired,
	// and one a test that only counts messages would pass on.
	var got [][]byte
	for len(got) < ansaClearRepeats+1 {
		select {
		case d := <-raw:
			got = append(got, d)
		case <-time.After(4 * time.Second):
			t.Fatalf("only %d rawinput messages arrived", len(got))
		}
	}
	for i := 0; i < ansaClearRepeats; i++ {
		_, m := ansaDecode(t, codecs, got[i])
		if k := m.(*msg.MsgRawKeyInput); string(k.Data) != string(ansaClearComposerKey) {
			t.Fatalf("key %d = %q, want a clear before the text", i, string(k.Data))
		}
	}
	_, mt := ansaDecode(t, codecs, got[ansaClearRepeats])
	if k := mt.(*msg.MsgRawKeyInput); string(k.Data) != "the work order" {
		t.Errorf("key after the clears = %q, want the text", string(k.Data))
	}
}

// A pane with no foreground program takes the inbox path and must not be typed
// into at all, so no clear is sent either. F-25's two-kinds-of-target rule still
// holds after this change.
func TestAPaneWithNoProgramIsStillNotTypedInto(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const pane = "pane-uuid-bare-shell"
	raw := make(chan []byte, 8)
	sub, err := nc.Subscribe(msg.T("pane", pane, "rawinput"), func(m *nats.Msg) { raw <- m.Data })
	if err != nil {
		t.Fatalf("subscribe rawinput: %v", err)
	}
	defer sub.Unsubscribe()

	if err := ansaDeliverToInbox(pub, pane, msg.AnsaModePrompt, "run this", ""); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	select {
	case d := <-raw:
		_, m := ansaDecode(t, codecs, d)
		t.Fatalf("a shell-less pane was sent raw input (%T) — the clear leaked onto the "+
			"path that must stay on the inbox", m)
	case <-time.After(700 * time.Millisecond):
	}
}

// ---------------------------------------------------------------------------
// F-38 — the composer is BETWEEN the last two rules
// ---------------------------------------------------------------------------

// A real 48-column fleet pane, after an order was typed and the CR absorbed.
// The order is in the composer; the footer below it never contains the order.
func paneWithOrderInComposer(order string) []string {
	return []string{
		"  transcript line from an earlier turn",
		"✻ Baked for 6m 4s",
		"",
		"─────────────────────────────────────────────",
		"❯ " + order,
		"  wrapped continuation of the same order",
		"─────────────────────────────────────────────",
		"  ⏵⏵ bypass permissions on (shift+tab to     ·",
	}
}

// THE DEFECT: scanning from the LAST rule sees only the footer, so the order is
// never found, ansaConfirmSubmitted concludes it was submitted, and the re-press
// never fires. F-35 shipped as a no-op because of this.
func TestAnUnsubmittedOrderIsFoundInTheComposer(t *testing.T) {
	rows := paneWithOrderInComposer("Print TWENTY essays of 3000 words")
	if !ansaComposerHolds(rows, "Print TWENTY essays") {
		t.Error("an order sitting unsent in the composer was reported as submitted — " +
			"the re-press never fires and the work order silently never runs")
	}
}

// The mirror: once the order is submitted it moves ABOVE the composer, into the
// transcript. Finding it there would make the check re-press forever.
func TestASubmittedOrderIsNotMistakenForAnUnsentOne(t *testing.T) {
	rows := []string{
		"❯ Print TWENTY essays of 3000 words",
		"⏺ working on it",
		"─────────────────────────────────────────────",
		"❯ ",
		"─────────────────────────────────────────────",
		"  ⏵⏵ bypass permissions on (shift+tab to     ·",
	}
	if ansaComposerHolds(rows, "Print TWENTY essays") {
		t.Error("an order echoed in the TRANSCRIPT was read as still in the composer — " +
			"this would re-press Enter forever on a working pane")
	}
}

// Footer text must never satisfy the check either.
func TestTheFooterIsNotTheComposer(t *testing.T) {
	rows := []string{
		"  some output",
		"─────────────────────────────────────────────",
		"❯ ",
		"─────────────────────────────────────────────",
		"  ⏵⏵ bypass permissions on · esc to interrupt",
	}
	if ansaComposerHolds(rows, "esc to interrupt") {
		t.Error("the footer was treated as composer content")
	}
}

// A pane with no composer drawn at all — a shell, or a program on the alternate
// screen — answers "not held" rather than guessing.
func TestNoComposerMeansNotHeld(t *testing.T) {
	if ansaComposerHolds([]string{"$ ls", "a  b  c"}, "ls") {
		t.Error("a pane with no composer rules was treated as holding an order")
	}
}

// F-39: a TRANSIENT screen-read failure must not be read as "submitted".
//
// The first version returned on any read error, reasoning that the publish had
// already succeeded. But the VT request has a 2 s budget and a loaded session
// misses it — which is exactly when a fleet is being driven hard. So the
// give-up fired precisely when verification mattered, and orders stayed in
// composers while every receipt said delivered. Seen on camera: a worker
// answered a probe, took the order, and never ran it.
func TestATransientScreenFailureDoesNotCountAsSubmitted(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const pane = "pane-flaky-read"
	raw := make(chan []byte, 32)
	sub, err := nc.Subscribe(msg.T("pane", pane, "rawinput"), func(m *nats.Msg) { raw <- m.Data })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	held := []string{
		"────────────────────────────────",
		"❯ Print TWENTY essays of 3000 words",
		"────────────────────────────────",
		"  ⏵⏵ bypass permissions on",
	}
	// Two reads fail, then the composer still holds the order: the re-press
	// must happen anyway.
	tr := &fakeAnsaTransport{screenFailFirst: 2, screens: [][]string{held}}

	prev := ansaSubmitRecheck
	ansaSubmitRecheck = time.Millisecond
	defer func() { ansaSubmitRecheck = prev }()

	if err := ansaConfirmSubmitted(pub, tr.Screen, pane, "Print TWENTY essays of 3000 words", "claude"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	select {
	case <-raw: // at least one re-press reached the wire
	case <-time.After(2 * time.Second):
		t.Error("a transient read failure abandoned verification — the order stays unsent " +
			"and the sender is told it was delivered")
	}
}
