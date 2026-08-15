// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// stubLinkableAdapter is a ChannelAdapter + LinkableChannel that records
// TriggerLink calls, standing in for SignalAdapter under `##humanoid pair link`.
type stubLinkableAdapter struct {
	inbound   chan channels.InboundMessage
	triggered []bool // force flag per TriggerLink call
}

func (s *stubLinkableAdapter) Type() string                                         { return "signal" }
func (s *stubLinkableAdapter) Start(context.Context) error                          { return nil }
func (s *stubLinkableAdapter) Stop() error                                          { return nil }
func (s *stubLinkableAdapter) Send(context.Context, channels.OutboundMessage) error { return nil }
func (s *stubLinkableAdapter) InboundCh() <-chan channels.InboundMessage            { return s.inbound }
func (s *stubLinkableAdapter) Status() msg.ChannelStatus                            { return msg.ChannelStatus{Type: "signal"} }
func (s *stubLinkableAdapter) SetReplyMode(string)                                  {}
func (s *stubLinkableAdapter) TriggerLink(force bool) error {
	s.triggered = append(s.triggered, force)
	return nil
}

var _ channels.ChannelAdapter = (*stubLinkableAdapter)(nil)
var _ channels.LinkableChannel = (*stubLinkableAdapter)(nil)

// TestHumanoidPairLinkTriggersAdapter: MsgChannelPairLink reaches the running
// adapter's TriggerLink with the force flag intact — the actor half of
// `##humanoid pair link <name> <channel> [force]` (X4, design 009 §3.4).
func TestHumanoidPairLinkTriggersAdapter(t *testing.T) {
	h, nc := newHumanoidForPairingTest(t, map[string]msg.ChannelConfig{})
	defer nc.Close()
	stub := &stubLinkableAdapter{inbound: make(chan channels.InboundMessage)}
	h.adapters = map[string]channels.ChannelAdapter{"signal": stub}

	h.handlePairLink(&msg.MsgChannelPairLink{HumanoidName: "gate-bot", Channel: "signal"})
	h.handlePairLink(&msg.MsgChannelPairLink{HumanoidName: "gate-bot", Channel: "signal", Force: true})

	if len(stub.triggered) != 2 || stub.triggered[0] != false || stub.triggered[1] != true {
		t.Fatalf("TriggerLink calls = %v, want [false true]", stub.triggered)
	}
}

// TestHumanoidPairLinkIgnoresWrongTargets: a link request for another
// humanoid, an unknown channel, or a channel that cannot device-link must
// never reach TriggerLink (and must not panic on the nil/missing adapter).
func TestHumanoidPairLinkIgnoresWrongTargets(t *testing.T) {
	h, nc := newHumanoidForPairingTest(t, map[string]msg.ChannelConfig{})
	defer nc.Close()
	stub := &stubLinkableAdapter{inbound: make(chan channels.InboundMessage)}
	h.adapters = map[string]channels.ChannelAdapter{"signal": stub}

	// Wrong humanoid.
	h.handlePairLink(&msg.MsgChannelPairLink{HumanoidName: "someone-else", Channel: "signal"})
	// Channel not running.
	h.handlePairLink(&msg.MsgChannelPairLink{HumanoidName: "gate-bot", Channel: "email"})

	if len(stub.triggered) != 0 {
		t.Fatalf("TriggerLink calls = %v, want none", stub.triggered)
	}
}
