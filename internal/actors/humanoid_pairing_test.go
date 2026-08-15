// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newHumanoidForPairingTest builds a minimal HumanoidActor over an in-process
// NATS publisher and a file-backed PairingStore rooted in a temp dir (t.Chdir),
// gated per the supplied per-channel configs. activeChatPaneID stays empty so
// pane echoes are skipped unless a test sets it.
func newHumanoidForPairingTest(t *testing.T, contacts map[string]msg.ChannelConfig) (*HumanoidActor, *nats.Conn) {
	t.Helper()
	t.Chdir(t.TempDir())
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	h := &HumanoidActor{
		name:               "gate-bot",
		active:             true,
		pub:                pub,
		nc:                 nc,
		llmPromptExecInbox: msg.T("pane", "gate-bot", "llm_prompt_execution", "inbox"),
		conversations:      make(map[string]*ConversationContext),
		contacts:           contacts,
		pairing:            channels.NewPairingStore(nil), // file fallback — no JetStream needed
	}
	return h, nc
}

// natsEnvelope mirrors the wire envelope (sharedmsg.NATSEnvelope) for
// decoding captured publishes.
type natsEnvelope struct {
	TypeTag string          `json:"t"`
	Payload json.RawMessage `json:"p"`
}

// drainSubject collects every message published on sub within the window,
// returning the decoded envelopes.
func drainSubject(t *testing.T, sub *nats.Subscription, window time.Duration) []natsEnvelope {
	t.Helper()
	var envs []natsEnvelope
	deadline := time.Now().Add(window)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return envs
		}
		m, err := sub.NextMsg(remaining)
		if err != nil {
			return envs
		}
		var env natsEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		envs = append(envs, env)
	}
}

func pairInbound(sender, name, content string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "slack",
		SenderID:    sender,
		SenderName:  name,
		ThreadID:    "t1",
		Content:     content,
	}
}

// TestHumanoidPairingGateRequest drives the full unknown→pending→approve→served
// flow on a gated channel with the default "request" policy: the unknown sender
// never reaches the LLM and produces exactly one MsgChannelPairRequest; after
// approval the next inbound is served.
func TestHumanoidPairingGateRequest(t *testing.T) {
	h, nc := newHumanoidForPairingTest(t, map[string]msg.ChannelConfig{
		"slack": {Enabled: true, PairingPolicy: "request"},
	})

	pairingSub, err := nc.SubscribeSync(msg.T("humanoid", "gate-bot", "pairing"))
	if err != nil {
		t.Fatalf("subscribe pairing: %v", err)
	}
	llmSub, err := nc.SubscribeSync(h.llmPromptExecInbox)
	if err != nil {
		t.Fatalf("subscribe llm inbox: %v", err)
	}

	h.handleInboundMessage(pairInbound("U-stranger", "Mallory", "hello?"))

	// Fail-closed: nothing queued, nothing in flight, no outbound context.
	if h.inboundProcessing || len(h.inboundQueue) != 0 || h.lastInbound != nil {
		t.Fatalf("unknown sender leaked into the inbound path: processing=%v queue=%d last=%v",
			h.inboundProcessing, len(h.inboundQueue), h.lastInbound)
	}

	// Exactly one MsgChannelPairRequest, nothing on the LLM inbox.
	envs := drainSubject(t, pairingSub, 500*time.Millisecond)
	if len(envs) != 1 || envs[0].TypeTag != msg.TagChannelPairRequest {
		t.Fatalf("expected exactly one MsgChannelPairRequest, got %+v", envs)
	}
	var req msg.MsgChannelPairRequest
	if err := json.Unmarshal(envs[0].Payload, &req); err != nil {
		t.Fatalf("decode pair request: %v", err)
	}
	if req.SenderID != "U-stranger" || req.Channel != "slack" || len(req.Code) != 6 {
		t.Fatalf("unexpected pair request: %+v", req)
	}
	if got := drainSubject(t, llmSub, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("unknown sender must never reach the LLM, got %+v", got)
	}

	// Approve (channel omitted, as the terminal command sends it) → promoted.
	h.handlePairApprove(&msg.MsgChannelPairApprove{HumanoidName: "gate-bot", Code: req.Code})
	if !h.pairing.Allowed("gate-bot", "slack", "U-stranger") {
		t.Fatal("approve did not promote the sender to the allowlist")
	}

	// Next inbound from the now-approved sender is served.
	h.handleInboundMessage(pairInbound("U-stranger", "Mallory", "hello again"))
	if !h.inboundProcessing || h.lastInbound == nil {
		t.Fatal("approved sender was not served")
	}
	if got := drainSubject(t, llmSub, 500*time.Millisecond); len(got) == 0 {
		t.Fatal("approved sender's message did not produce an LLM prompt")
	}
}

// TestHumanoidPairingGateDrop verifies the "drop" policy: unknown senders
// vanish with no pending state, no pairing request, and no LLM dispatch.
func TestHumanoidPairingGateDrop(t *testing.T) {
	h, nc := newHumanoidForPairingTest(t, map[string]msg.ChannelConfig{
		"slack": {Enabled: true, PairingPolicy: "drop"},
	})
	pairingSub, _ := nc.SubscribeSync(msg.T("humanoid", "gate-bot", "pairing"))
	llmSub, _ := nc.SubscribeSync(h.llmPromptExecInbox)

	h.handleInboundMessage(pairInbound("U-stranger", "Mallory", "hi"))

	if h.inboundProcessing || len(h.inboundQueue) != 0 {
		t.Fatal("dropped sender leaked into the inbound path")
	}
	if got := drainSubject(t, pairingSub, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("drop policy must not create pairing requests, got %+v", got)
	}
	if got := drainSubject(t, llmSub, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("drop policy must not dispatch to the LLM, got %+v", got)
	}
	rec, _ := h.pairing.List("gate-bot", "slack")
	if len(rec.Pending) != 0 {
		t.Fatalf("drop policy must not record pendings, got %d", len(rec.Pending))
	}
}

// TestHumanoidPairingUngatedChannel verifies backward compatibility: a channel
// with no allowlist and no pairing_policy admits every sender exactly as
// before WS3.
func TestHumanoidPairingUngatedChannel(t *testing.T) {
	h, _ := newHumanoidForPairingTest(t, map[string]msg.ChannelConfig{
		"slack": {Enabled: true},
	})

	h.handleInboundMessage(pairInbound("U-anyone", "Anyone", "hi"))
	if !h.inboundProcessing || h.lastInbound == nil || h.lastInbound.SenderID != "U-anyone" {
		t.Fatal("ungated channel must admit unknown senders (pre-WS3 behaviour)")
	}
}

// TestHumanoidPairingDeclaredAllowlist verifies the spawn-time merge: senders
// declared in the channel's allowlist are admitted, others still gate.
func TestHumanoidPairingDeclaredAllowlist(t *testing.T) {
	h, _ := newHumanoidForPairingTest(t, map[string]msg.ChannelConfig{
		"slack": {Enabled: true, Allowlist: []string{"U-friend"}},
	})
	h.mergeDeclaredAllowlists() // Started does this at spawn

	h.handleInboundMessage(pairInbound("U-friend", "Friend", "hi"))
	if !h.inboundProcessing {
		t.Fatal("declared-allowlisted sender was not served")
	}

	// Reset the in-flight state and probe an unknown sender on the same channel.
	h.advanceInboundQueue()
	h.lastInbound = nil
	h.handleInboundMessage(pairInbound("U-stranger", "Mallory", "hi"))
	if h.inboundProcessing || h.lastInbound != nil {
		t.Fatal("non-allowlisted sender must gate once an allowlist is declared")
	}

	// A second merge stays idempotent.
	h.mergeDeclaredAllowlists()
	rec, _ := h.pairing.List("gate-bot", "slack")
	if len(rec.Allowlist) != 1 {
		t.Fatalf("allowlist merge not idempotent: %v", rec.Allowlist)
	}
}

// stubPairingAdapter is a minimal ChannelAdapter + PairingChannel used to
// drive pairingLoop the way a QR device-link adapter would.
type stubPairingAdapter struct {
	events  chan channels.PairingEvent
	inbound chan channels.InboundMessage
}

func (s *stubPairingAdapter) Type() string                                         { return "whatsapp" }
func (s *stubPairingAdapter) Start(context.Context) error                          { return nil }
func (s *stubPairingAdapter) Stop() error                                          { return nil }
func (s *stubPairingAdapter) Send(context.Context, channels.OutboundMessage) error { return nil }
func (s *stubPairingAdapter) InboundCh() <-chan channels.InboundMessage            { return s.inbound }
func (s *stubPairingAdapter) Status() msg.ChannelStatus                            { return msg.ChannelStatus{Type: "whatsapp"} }
func (s *stubPairingAdapter) SetReplyMode(string)                                  {}
func (s *stubPairingAdapter) PairingCh() <-chan channels.PairingEvent              { return s.events }

var _ channels.ChannelAdapter = (*stubPairingAdapter)(nil)
var _ channels.PairingChannel = (*stubPairingAdapter)(nil)

// TestHumanoidPairingLoopQRThenLinked drives a stub PairingChannel through the
// qr→linked lifecycle and asserts: MsgChannelPairQR then a connected
// MsgChannelPairStatus are published on the pairing subject, the device-link
// blob is persisted via SaveDeviceLink, and the pane-echo handlers render the
// QR payload and the linked notice into the pane's rysh output.
func TestHumanoidPairingLoopQRThenLinked(t *testing.T) {
	h, nc := newHumanoidForPairingTest(t, map[string]msg.ChannelConfig{
		"whatsapp": {Enabled: true, PairingPolicy: "request"},
	})

	pairingSub, err := nc.SubscribeSync(msg.T("humanoid", "gate-bot", "pairing"))
	if err != nil {
		t.Fatalf("subscribe pairing: %v", err)
	}

	stub := &stubPairingAdapter{
		events:  make(chan channels.PairingEvent, 4),
		inbound: make(chan channels.InboundMessage),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.pairingLoop(ctx, "whatsapp", stub)

	blob := json.RawMessage(`{"session":"whatsmeow-creds"}`)
	stub.events <- channels.PairingEvent{Kind: "qr", QR: "2@abc123-linkpayload"}
	stub.events <- channels.PairingEvent{Kind: "linked", Session: blob}

	envs := drainSubject(t, pairingSub, time.Second)
	if len(envs) != 2 {
		t.Fatalf("expected QR + status publishes, got %+v", envs)
	}
	if envs[0].TypeTag != msg.TagChannelPairQR {
		t.Fatalf("first publish should be MsgChannelPairQR, got %s", envs[0].TypeTag)
	}
	var qr msg.MsgChannelPairQR
	if err := json.Unmarshal(envs[0].Payload, &qr); err != nil || qr.QR != "2@abc123-linkpayload" {
		t.Fatalf("bad QR payload: %+v err=%v", qr, err)
	}
	// The loop also pre-renders the dashboard PNG so the same publish feeds the
	// web card (X4, design 009).
	if !strings.HasPrefix(qr.QRImage, "data:image/png;base64,") {
		t.Fatalf("QRImage not a PNG data URI: %.40q", qr.QRImage)
	}
	if envs[1].TypeTag != msg.TagChannelPairStatus {
		t.Fatalf("second publish should be MsgChannelPairStatus, got %s", envs[1].TypeTag)
	}
	var st msg.MsgChannelPairStatus
	if err := json.Unmarshal(envs[1].Payload, &st); err != nil || !st.Connected {
		t.Fatalf("bad status: %+v err=%v", st, err)
	}

	// The device-link creds persisted (poll: SaveDeviceLink runs on the loop
	// goroutine just before the status publish, so it is already durable here,
	// but keep a small retry for scheduler slack).
	deadline := time.Now().Add(time.Second)
	for {
		if got, ok := h.pairing.LoadDeviceLink("gate-bot", "whatsapp"); ok && bytes.Equal(got, blob) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("device link was not persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Pane rendering: the Receive-side handlers echo into the pane's rysh
	// output (the bridge loops the publishes back in production).
	h.activeChatPaneID = "pane-qr"
	paneSub, err := nc.SubscribeSync(msg.T("pane", "pane-qr", "output", "rysh"))
	if err != nil {
		t.Fatalf("subscribe pane output: %v", err)
	}
	h.handlePairQR(&qr)
	h.handlePairStatus(&st)
	var paneText strings.Builder
	for _, env := range drainSubject(t, paneSub, 500*time.Millisecond) {
		paneText.Write(env.Payload)
	}
	if !strings.Contains(paneText.String(), "2@abc123-linkpayload") {
		t.Fatalf("pane did not receive the QR payload: %s", paneText.String())
	}
	if !strings.Contains(paneText.String(), "whatsapp linked") {
		t.Fatalf("pane did not receive the linked notice: %s", paneText.String())
	}
}
