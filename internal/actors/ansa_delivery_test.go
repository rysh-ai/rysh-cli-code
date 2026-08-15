// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Tests for F-25: ANSA delivered to a pane running claude by handing rysh's own
// LLMActor a prompt, which never reaches the program that owns the PTY. Every
// send reported success and no agent ever woke — a five-agent fleet died at hop
// one, and W4 proposed deleting the typing path on the strength of it.
//
// These assert ON THE WIRE, because the wire is where the defect was: the
// router, the resolver and the doors were all correct and the bytes went to a
// subject the target does not read.

// ansaDecode pulls the type tag and message out of a published NATSEnvelope.
func ansaDecode(t *testing.T, codecs *msg.CodecRegistry, data []byte) (string, interface{}) {
	t.Helper()
	var env msg.NATSEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	m, err := codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		t.Fatalf("decode %s: %v", env.TypeTag, err)
	}
	return env.TypeTag, m
}

// TestAnsaTypesIntoAPaneRunningAProgram is the headline. A pane whose foreground
// program is claude must be reached the way a human reaches it — by typing into
// its PTY — and the CR must be a SEPARATE message, because an inline TUI treats
// a burst containing a newline as a paste and leaves it unsent in the composer.
func TestAnsaTypesIntoAPaneRunningAProgram(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const pane = "pane-uuid-running-claude"
	raw := make(chan []byte, 8)
	inbox := make(chan []byte, 8)
	sub1, err := nc.Subscribe(msg.T("pane", pane, "rawinput"), func(m *nats.Msg) { raw <- m.Data })
	if err != nil {
		t.Fatalf("subscribe rawinput: %v", err)
	}
	defer sub1.Unsubscribe()
	sub2, err := nc.Subscribe(msg.T("pane", pane, "inbox"), func(m *nats.Msg) { inbox <- m.Data })
	if err != nil {
		t.Fatalf("subscribe inbox: %v", err)
	}
	defer sub2.Unsubscribe()

	if err := ansaDeliverToInbox(pub, pane, msg.AnsaModePrompt, "build the backend now", "claude"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	// THREE messages since F-31: clear, text, CR. The clear was added when a
	// drained stop left a cancelled order in the composer and the next work
	// order arrived fused to it.
	var got [][]byte
	for len(got) < ansaClearRepeats+2 {
		select {
		case d := <-raw:
			got = append(got, d)
		case <-time.After(3 * time.Second):
			t.Fatalf("want %d rawinput messages (N clears, text, CR), got %d — a pane running a "+
				"program is not being typed into, so the agent never wakes (F-25)", ansaClearRepeats+2, len(got))
		}
	}

	_, mc := ansaDecode(t, codecs, got[0])
	kc, ok := mc.(*msg.MsgRawKeyInput)
	if !ok {
		t.Fatalf("first message is %T, want *msg.MsgRawKeyInput", mc)
	}
	if string(kc.Data) != string(ansaClearComposerKey) {
		t.Errorf("first key = %q, want the composer clear (F-31): typing appends to "+
			"whatever is on the line, so a delivery must start from an empty one", string(kc.Data))
	}

	_, m0 := ansaDecode(t, codecs, got[ansaClearRepeats])
	k0, ok := m0.(*msg.MsgRawKeyInput)
	if !ok {
		t.Fatalf("second message is %T, want *msg.MsgRawKeyInput", m0)
	}
	if string(k0.Data) != "build the backend now" {
		t.Errorf("typed text = %q, want the message text", string(k0.Data))
	}

	_, m1 := ansaDecode(t, codecs, got[ansaClearRepeats+1])
	k1, ok := m1.(*msg.MsgRawKeyInput)
	if !ok {
		t.Fatalf("third message is %T, want *msg.MsgRawKeyInput", m1)
	}
	if string(k1.Data) != "\r" {
		t.Errorf("submit key = %q, want a bare CR. Bundling it with the text is the "+
			"documented trap: the TUI reads the burst as a PASTE and the message sits "+
			"in the composer unsent, looking delivered", string(k1.Data))
	}

	select {
	case d := <-inbox:
		tag, _ := ansaDecode(t, codecs, d)
		t.Errorf("a pane running claude also received %s on its inbox — that path drives "+
			"rysh's own LLMActor and never reaches the program holding the PTY", tag)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestAnsaUsesTheInboxForAPaneWithNoProgram guards the other half. A bare rysh
// pane has no program on the PTY, so the original inbox path is correct for it —
// the fix must not turn every delivery into keystrokes.
func TestAnsaUsesTheInboxForAPaneWithNoProgram(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const pane = "pane-uuid-bare-shell"
	raw := make(chan []byte, 4)
	inbox := make(chan []byte, 4)
	s1, _ := nc.Subscribe(msg.T("pane", pane, "rawinput"), func(m *nats.Msg) { raw <- m.Data })
	defer s1.Unsubscribe()
	s2, _ := nc.Subscribe(msg.T("pane", pane, "inbox"), func(m *nats.Msg) { inbox <- m.Data })
	defer s2.Unsubscribe()

	if err := ansaDeliverToInbox(pub, pane, msg.AnsaModePrompt, "summarise the log", ""); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	select {
	case d := <-inbox:
		tag, _ := ansaDecode(t, codecs, d)
		if tag != msg.TagPaneExecPrompt {
			t.Errorf("bare pane got %s, want %s", tag, msg.TagPaneExecPrompt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a pane with no foreground program received nothing on its inbox")
	}

	select {
	case <-raw:
		t.Error("a bare rysh pane was sent keystrokes; only a pane owned by a program " +
			"should be typed into")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestAnsaProbeCarriesTheProgramToDelivery pins the plumbing, not the policy.
// Probe already fetches the pane snapshot, so the program rides back on that
// SAME round trip — F-23 was caused by one fact having two sources, and this
// keeps the how-to-deliver fact single-sourced.
func TestAnsaProbeCarriesTheProgramToDelivery(t *testing.T) {
	tr := &fakeAnsaTransport{
		panes:   []ansaPane{{ID: "pane-a", GivenName: "worker-1"}},
		program: "claude",
	}
	r := &ansaRouter{tr: tr}

	res := r.Route(&msg.MsgAnsaRoute{V: msg.AnsaSchemaVersion, To: "pane-a", Mode: msg.AnsaModePrompt, Text: "go"})
	if !res.OK {
		t.Fatalf("route failed: %+v", res)
	}
	if len(tr.delivered) != 1 {
		t.Fatalf("want exactly 1 delivery, got %d", len(tr.delivered))
	}
	if tr.delivered[0].Program != "claude" {
		t.Errorf("Deliver saw program %q, want %q — the probe's answer is not reaching "+
			"the delivery decision, so it cannot choose to type", tr.delivered[0].Program, "claude")
	}
}
