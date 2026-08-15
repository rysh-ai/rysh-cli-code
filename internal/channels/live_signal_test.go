// SPDX-License-Identifier: Apache-2.0

//go:build livechannels

package channels

// LV1 Signal proofs (P-3, design 010; device link is X4, design 009).
//
// Two independent proofs live here because Signal is two mechanisms wearing one
// adapter:
//
//   - TestLiveSignalSend / TestLiveSignalRoundTrip drive the message path against
//     a REAL `signal-cli` daemon holding a REAL linked number.
//   - TestLiveSignalDeviceLinkQR drives the device-link producer (startLink →
//     sgnl:// URI → finishLink) against a REAL account-less daemon.
//
// What the existing fixture tests already cover, so these must not re-prove it:
// signal_test.go stands up a FAKE JSON-RPC daemon on a real UNIX socket
// (fakeSignalDaemon) and covers envelope→InboundMessage mapping, send params,
// group routing, RPC errors and step suppression; signal_link_test.go stands up
// fakeLinkDaemon and covers the qr→linked event sequence, the finishLink error
// path, the re-link guard and TriggerLink. Everything below the wire is
// therefore already green. What is NOT proven by any of it is that a real
// signal-cli daemon speaks the dialect the adapter assumes — that its "receive"
// notifications carry `envelope.dataMessage.message`, that `send` is accepted by
// Signal itself, and that `startLink` exists on the operator's signal-cli build.
// That gap is exactly what these tests close.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// liveSignalMask reduces a phone number to a loggable form — last four digits
// only. Test logs land in CI artifacts, so no full identity is ever printed.
func liveSignalMask(number string) string {
	if len(number) <= 4 {
		return "****"
	}
	return "…" + number[len(number)-4:]
}

// liveSignalNextPairing waits for the next device-link event, failing at
// timeout. Kept local to this file (rule: no edits to the shared live_test.go
// while sibling workers are in the package).
func liveSignalNextPairing(t *testing.T, ch <-chan PairingEvent, timeout time.Duration) PairingEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(timeout):
		t.Fatalf("no pairing event within %s", timeout)
		return PairingEvent{}
	}
}

// liveSignalAdapter builds and starts a SignalAdapter attached to a real
// signal-cli daemon, registering Stop as cleanup.
func liveSignalAdapter(t *testing.T, cfg msg.ChannelConfig) *SignalAdapter {
	t.Helper()
	adapter := NewSignalAdapter(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("signal Start (dial sidecar): %v", err)
	}
	t.Cleanup(func() { _ = adapter.Stop() })
	if st := adapter.Status(); !st.Connected {
		t.Fatalf("signal adapter not connected after Start: %s", st.Error)
	}
	return adapter
}

// TestLiveSignalSend proves the outbound half of the Signal row: the adapter
// attaches to a real `signal-cli` JSON-RPC daemon and Signal itself accepts a
// "send" for a second number. No human needs to be holding the second phone at
// the moment of the run — the probe simply lands on it.
//
// Env:
//
//	RYSH_LIVE_SIGNAL_SIDECAR_ADDR   JSON-RPC endpoint of a running signal-cli
//	                                daemon: a UNIX socket path ("/run/signal.sock")
//	                                or "host:port". Adapter requirement, not optional.
//	RYSH_LIVE_SIGNAL_NUMBER         the E.164 number the daemon holds ("+1555…").
//	                                Becomes the "account" param on every send.
//	RYSH_LIVE_SIGNAL_ECHO_NUMBER    a SECOND E.164 number, on a different phone,
//	                                that can receive from the first.
//
// What a human must do first:
//
//	 1. Install signal-cli (>= 0.11, the JSON-RPC daemon builds) on the machine
//	    that will run this test.
//	 2. Register or link RYSH_LIVE_SIGNAL_NUMBER under signal-cli's data dir —
//	    the adapter never does this for the message path (signal.go's header
//	    comment: "this adapter assumes the number is already registered/linked").
//	    Linking is what TestLiveSignalDeviceLinkQR below covers.
//	 3. Run the daemon so it both serves RPC and pushes inbound envelopes:
//	    `signal-cli -a <NUMBER> daemon --socket <PATH>`  (or `--tcp host:port`).
//	    The daemon must be in a receiving mode — the adapter has no polling
//	    fallback, it only consumes unsolicited "receive" notifications.
//	 4. Own a second Signal-registered phone number and have sent at least one
//	    message between the two, so neither side is a fresh unknown contact.
func TestLiveSignalSend(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_SIGNAL_SIDECAR_ADDR",
		"RYSH_LIVE_SIGNAL_NUMBER",
		"RYSH_LIVE_SIGNAL_ECHO_NUMBER",
	)
	number := env["RYSH_LIVE_SIGNAL_NUMBER"]
	echo := env["RYSH_LIVE_SIGNAL_ECHO_NUMBER"]

	adapter := liveSignalAdapter(t, msg.ChannelConfig{
		Enabled:     true,
		SidecarAddr: env["RYSH_LIVE_SIGNAL_SIDECAR_ADDR"],
		Number:      number,
	})
	t.Logf("attached to signal-cli daemon; account=%s", liveSignalMask(number))

	nonce := fmt.Sprintf("rysh-lv1-signal-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := adapter.Send(ctx, OutboundMessage{
		RecipientID: echo,
		Content:     "rysh LV1 probe " + nonce,
	}); err != nil {
		t.Fatalf("signal Send to %s: %v", liveSignalMask(echo), err)
	}
	t.Logf("send accepted by signal-cli: to=%s nonce_len=%d", liveSignalMask(echo), len(nonce))
}

// TestLiveSignalRoundTrip closes the loop: the adapter sends a nonce-carrying
// probe to the second number and a HUMAN on that phone replies with the nonce,
// which must surface on InboundCh via the daemon's "receive" notification.
//
// Signal has no bot API, so there is no way to automate the far side — the
// second identity is a person. The test therefore only runs when the operator
// asserts a human is standing by, by setting RYSH_LIVE_SIGNAL_AWAIT_ECHO=1.
// Without it the test skips, which is what keeps the nightly green.
//
// Env: everything TestLiveSignalSend needs, plus
//
//	RYSH_LIVE_SIGNAL_AWAIT_ECHO       set to any non-empty value to opt in —
//	                                  it means "a human is watching the second
//	                                  phone right now and will reply".
//	RYSH_LIVE_SIGNAL_WAIT_SECS        seconds to wait for the reply (optional,
//	                                  default 90).
//
// What the human does: watch the second phone, and when the probe arrives reply
// to it with any text that CONTAINS the nonce printed in the test log. Copying
// the whole probe message back is the simplest way.
func TestLiveSignalRoundTrip(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_SIGNAL_SIDECAR_ADDR",
		"RYSH_LIVE_SIGNAL_NUMBER",
		"RYSH_LIVE_SIGNAL_ECHO_NUMBER",
		"RYSH_LIVE_SIGNAL_AWAIT_ECHO",
	)
	number := env["RYSH_LIVE_SIGNAL_NUMBER"]
	echo := env["RYSH_LIVE_SIGNAL_ECHO_NUMBER"]

	adapter := liveSignalAdapter(t, msg.ChannelConfig{
		Enabled:     true,
		SidecarAddr: env["RYSH_LIVE_SIGNAL_SIDECAR_ADDR"],
		Number:      number,
	})

	nonce := fmt.Sprintf("rysh-lv1-signal-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probe := "rysh LV1 round-trip probe " + nonce + " — reply with this text"
	if err := adapter.Send(ctx, OutboundMessage{RecipientID: echo, Content: probe}); err != nil {
		t.Fatalf("signal Send to %s: %v", liveSignalMask(echo), err)
	}
	wait := time.Duration(envInt("RYSH_LIVE_SIGNAL_WAIT_SECS", 90)) * time.Second
	t.Logf("probe sent to %s; reply within %s with text containing: %s",
		liveSignalMask(echo), wait, nonce)

	got := awaitInbound(t, adapter.InboundCh(), wait, func(m InboundMessage) bool {
		return strings.Contains(m.Content, nonce)
	})
	// SenderID is envelope.source (an E.164 number) when the daemon reports one,
	// and the ACI uuid otherwise — both are valid, so this is a log, not a gate.
	t.Logf("round-trip OK: sender=%s thread_is_group=%t content_len=%d",
		liveSignalMask(got.SenderID), got.Metadata["group_id"] != "", len(got.Content))
}

// TestLiveSignalDeviceLinkQR proves the X4 device-link producer (design 009
// §3.1) against a real signal-cli — the half of Signal that has never touched a
// real daemon. With config.Link set, Start runs the link flow on its own
// goroutine: listAccounts (re-link guard) → startLink → a "qr" PairingEvent
// carrying the sgnl:// device-link URI → finishLink, which blocks until a phone
// scans and approves.
//
// The QR half is fully automated: it needs no phone, only a daemon that exposes
// startLink. That alone is worth running, because it is the one thing the fake
// daemon cannot tell us — whether the operator's signal-cli build has the method
// at all (design 009 §6 flags this: older builds only expose `link` as a CLI
// subcommand, not a JSON-RPC method).
//
// Env:
//
//	RYSH_LIVE_SIGNAL_LINK_SIDECAR_ADDR   JSON-RPC endpoint of an ACCOUNT-LESS
//	                                     signal-cli daemon — started WITHOUT
//	                                     `-a <number>`. startLink/finishLink are
//	                                     only exposed by an account-less daemon
//	                                     (design 009 §3.1), so this is deliberately
//	                                     a DIFFERENT daemon from the one the
//	                                     message-path tests attach to.
//	RYSH_LIVE_SIGNAL_LINK_AWAIT_SCAN     optional; set to any non-empty value when
//	                                     a human with a Signal phone is standing by
//	                                     to scan. Enables the second half (the
//	                                     "linked" event) AND prints the QR raster.
//	RYSH_LIVE_SIGNAL_LINK_SCAN_WAIT_SECS optional; seconds to wait for the scan
//	                                     (default 90; the adapter's own finishLink
//	                                     ceiling is 5 minutes).
//
// What a human must do first: run `signal-cli daemon --socket <PATH>` with NO
// `-a` flag, against a data dir that holds NO registered account — the re-link
// guard refuses (as an "error" event, not a link) when listAccounts reports one,
// and that refusal is deliberate: re-linking would re-provision a live device.
//
// SECRET NOTE: the sgnl:// URI is a capability — anyone who scans it before the
// real phone does links themselves onto the account. It is never logged, and the
// scannable raster is printed only under RYSH_LIVE_SIGNAL_LINK_AWAIT_SCAN, i.e.
// only when a human deliberately asked for something to scan at their terminal.
// Do not set that variable in CI.
func TestLiveSignalDeviceLinkQR(t *testing.T) {
	env := requireEnv(t, "RYSH_LIVE_SIGNAL_LINK_SIDECAR_ADDR")

	// Link=true and no Number: the link flow is what discovers the number, and
	// Start's "number is required" check is waived exactly for this case.
	adapter := liveSignalAdapter(t, msg.ChannelConfig{
		Enabled:     true,
		SidecarAddr: env["RYSH_LIVE_SIGNAL_LINK_SIDECAR_ADDR"],
		Link:        true,
	})

	ev := liveSignalNextPairing(t, adapter.PairingCh(), 30*time.Second)
	if ev.Kind == "error" {
		// The two expected shapes: an already-provisioned data dir (guard fired)
		// or a signal-cli too old to expose startLink. Both are operator-fixable,
		// so say which one it looks like.
		hint := "check the daemon was started account-less against an empty data dir"
		if strings.Contains(ev.Detail, "startLink") {
			hint = "signal-cli may predate the startLink JSON-RPC method (design 009 §6) — upgrade it"
		}
		t.Fatalf("device link failed before producing a QR: %s (%s)", ev.Detail, hint)
	}
	if ev.Kind != "qr" {
		t.Fatalf("first pairing event Kind = %q, want \"qr\"", ev.Kind)
	}
	// Accept both URI dialects: current signal-cli emits sgnl://linkdevice?…,
	// older builds emit tsdevice:/?…
	if !strings.HasPrefix(ev.QR, "sgnl:") && !strings.HasPrefix(ev.QR, "tsdevice:") {
		t.Fatalf("device-link URI has neither the sgnl: nor the tsdevice: scheme (len %d)", len(ev.QR))
	}
	t.Logf("startLink OK against a real daemon: scheme=%s payload_len=%d",
		strings.SplitN(ev.QR, ":", 2)[0], len(ev.QR))

	// Prove the payload is renderable by the shipped encoder — the terminal
	// raster is what a human actually scans (design 009 §3.2).
	raster, err := QRHalfBlocks(ev.QR)
	if err != nil {
		t.Fatalf("QRHalfBlocks on the live device-link URI: %v", err)
	}
	t.Logf("QR raster produced: %d rows", strings.Count(raster, "\n"))
	if _, err := QRPNGDataURI(ev.QR); err != nil {
		t.Fatalf("QRPNGDataURI on the live device-link URI: %v", err)
	}

	if env["RYSH_LIVE_SIGNAL_LINK_AWAIT_SCAN"] == "" {
		t.Log("QR half proven; set RYSH_LIVE_SIGNAL_LINK_AWAIT_SCAN=1 with a phone " +
			"standing by to also prove finishLink")
		return
	}

	// Only now — with a human explicitly asking for something to scan — print the
	// code itself. See the SECRET NOTE above.
	t.Logf("scan this with Signal → Settings → Linked devices:\n%s", raster)
	wait := time.Duration(envInt("RYSH_LIVE_SIGNAL_LINK_SCAN_WAIT_SECS", 90)) * time.Second
	linked := liveSignalNextPairing(t, adapter.PairingCh(), wait)
	if linked.Kind != "linked" {
		t.Fatalf("after the scan window, event Kind = %q detail = %q, want \"linked\"",
			linked.Kind, linked.Detail)
	}
	// Session is the linked account blob — a credential. Length only.
	t.Logf("device link OK: finishLink returned a session (%d bytes, not logged)", len(linked.Session))
}
