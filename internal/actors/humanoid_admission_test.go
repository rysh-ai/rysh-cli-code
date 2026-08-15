// SPDX-License-Identifier: Apache-2.0

package actors

// R7: admission-gate defaults (design 003 G5). The shipped default is
// fail-OPEN for backward compatibility, but it is now expressible, switchable
// and visible rather than implicit.

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func gateFor(cc msg.ChannelConfig, pairingDefault string) (bool, string) {
	h := &HumanoidActor{
		cfg:      config.Config{PairingDefault: pairingDefault},
		contacts: map[string]msg.ChannelConfig{"telegram": cc},
	}
	return h.channelPairingGate("telegram")
}

func TestPairingGateDeclaredPolicyOrAllowlist(t *testing.T) {
	// A declared allowlist gates, defaulting to the "request" policy.
	gated, policy := gateFor(msg.ChannelConfig{Allowlist: []string{"owner"}}, "")
	if !gated || policy != "request" {
		t.Errorf("allowlist should gate with request, got gated=%v policy=%q", gated, policy)
	}
	// An explicit drop policy is honoured.
	if gated, policy := gateFor(msg.ChannelConfig{PairingPolicy: "drop"}, ""); !gated || policy != "drop" {
		t.Errorf("drop policy = gated %v policy %q", gated, policy)
	}
}

// TestPairingGateDefaultIsOpenButSwitchable is the R7 contract: neither field
// declared follows the session default, which ships "open" and can be flipped.
func TestPairingGateDefaultIsOpenButSwitchable(t *testing.T) {
	bare := msg.ChannelConfig{}

	if gated, _ := gateFor(bare, ""); gated {
		t.Error("shipped default must stay open (pre-WS3 deployments keep answering)")
	}
	if gated, _ := gateFor(bare, "open"); gated {
		t.Error("explicit open must not gate")
	}
	gated, policy := gateFor(bare, "closed")
	if !gated || policy != "request" {
		t.Errorf("pairing_default=closed must gate with request, got gated=%v policy=%q", gated, policy)
	}
	// Case-insensitive, like every other config enum.
	if gated, _ := gateFor(bare, "CLOSED"); !gated {
		t.Error("pairing_default should be case-insensitive")
	}
}

// TestPairingPolicyOpenIsAnExplicitOptOut: `open` means "deliberately ungated"
// and must survive even when the session default is closed — otherwise there is
// no way to keep one public channel open on a locked-down session.
func TestPairingPolicyOpenOverridesClosedDefault(t *testing.T) {
	if gated, _ := gateFor(msg.ChannelConfig{PairingPolicy: "open"}, "closed"); gated {
		t.Error("an explicit `pairing_policy: open` must beat a closed session default")
	}
	// It also beats a declared allowlist: the operator said open outright.
	if gated, _ := gateFor(msg.ChannelConfig{PairingPolicy: "open", Allowlist: []string{"x"}}, ""); gated {
		t.Error("explicit open must not be overridden by a stale allowlist")
	}
}

func TestPairingGateUnknownChannel(t *testing.T) {
	h := &HumanoidActor{contacts: map[string]msg.ChannelConfig{}}
	if gated, _ := h.channelPairingGate("nope"); gated {
		t.Error("a channel with no contact block cannot be gated")
	}
}
