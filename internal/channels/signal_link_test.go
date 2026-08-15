// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// fakeLinkDaemon answers the signal-cli device-link RPCs (listAccounts /
// startLink / finishLink) on a socket so the X4 link producer can be driven
// without a real signal-cli. finishResult vs finishErr selects finishLink's
// outcome; accountsJSON is listAccounts' result ("" ⇒ no accounts, the fresh
// account-less daemon the link flow expects).
func fakeLinkDaemon(t *testing.T, ln net.Listener, linkURI, finishResult, finishErr, accountsJSON string) {
	t.Helper()
	if accountsJSON == "" {
		accountsJSON = `[]`
	}
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	dec := json.NewDecoder(conn)
	for {
		var req struct {
			Method string `json:"method"`
			ID     string `json:"id"`
		}
		if err := dec.Decode(&req); err != nil {
			return
		}
		var resp string
		switch req.Method {
		case "listAccounts":
			resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":%s}`, req.ID, accountsJSON)
		case "startLink":
			resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"deviceLinkUri":%q}}`, req.ID, linkURI)
		case "finishLink":
			if finishErr != "" {
				resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"error":{"code":-1,"message":%q}}`, req.ID, finishErr)
			} else {
				resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":%s}`, req.ID, finishResult)
			}
		default:
			continue
		}
		if _, err := conn.Write([]byte(resp + "\n")); err != nil {
			return
		}
	}
}

func startLinkAdapter(t *testing.T, linkURI, finishResult, finishErr string) *SignalAdapter {
	return startLinkAdapterAccounts(t, linkURI, finishResult, finishErr, "", true)
}

// startLinkAdapterAccounts is startLinkAdapter with control over the daemon's
// listAccounts result and whether Link (auto-link at Start) is set.
func startLinkAdapterAccounts(t *testing.T, linkURI, finishResult, finishErr, accountsJSON string, link bool) *SignalAdapter {
	t.Helper()
	// A short base dir: macOS caps unix-socket paths at ~104 chars, and the long
	// test names here overflow t.TempDir()'s path.
	dir, err := os.MkdirTemp("", "s")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go fakeLinkDaemon(t, ln, linkURI, finishResult, finishErr, accountsJSON)

	// Link=true starts without a Number (the link flow discovers it); the
	// on-demand path (link=false) models a config that declares the number up
	// front and provisions the daemon later via `##humanoid pair link`.
	cfg := msg.ChannelConfig{SidecarAddr: sock, Link: link}
	if !link {
		cfg.Number = "+15550000000"
	}
	a := NewSignalAdapter(cfg)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	return a
}

func nextPairing(t *testing.T, ch <-chan PairingEvent) PairingEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a pairing event")
		return PairingEvent{}
	}
}

// TestSignalLinkFlowEmitsQRThenLinked: with config.Link set, Start runs the
// device-link flow — startLink surfaces the QR, finishLink the linked session —
// on PairingCh, so the humanoid pairingLoop can render and persist it. This is
// the gap X4 closes: before it, no production adapter emitted pairing events.
func TestSignalLinkFlowEmitsQRThenLinked(t *testing.T) {
	const uri = "sgnl://linkdevice?uuid=TEST&pub_key=TEST"
	a := startLinkAdapter(t, uri, `{"number":"+15550009999"}`, "")

	qr := nextPairing(t, a.PairingCh())
	if qr.Kind != "qr" || qr.QR != uri {
		t.Fatalf("first event = %+v, want qr carrying %q", qr, uri)
	}
	linked := nextPairing(t, a.PairingCh())
	if linked.Kind != "linked" {
		t.Fatalf("second event = %+v, want linked", linked)
	}
	if !strings.Contains(string(linked.Session), "+15550009999") {
		t.Fatalf("linked session must carry the account to persist: %s", linked.Session)
	}
}

// TestSignalLinkFlowFinishError: a finishLink failure surfaces as an error event
// after the QR — never a false "linked".
func TestSignalLinkFlowFinishError(t *testing.T) {
	a := startLinkAdapter(t, "sgnl://linkdevice?uuid=X", "", "link declined")

	if ev := nextPairing(t, a.PairingCh()); ev.Kind != "qr" {
		t.Fatalf("first event = %+v, want qr", ev)
	}
	ev := nextPairing(t, a.PairingCh())
	if ev.Kind != "error" || !strings.Contains(ev.Detail, "link declined") {
		t.Fatalf("second event = %+v, want error carrying the reason", ev)
	}
}

// TestSignalAdapterIsPairingChannel makes the compile-time guarantee explicit:
// *SignalAdapter satisfies PairingChannel, so humanoid.startChannel's optional
// type-assert wires the pairingLoop for it — and LinkableChannel, so
// `##humanoid pair link` can trigger the flow on demand (design 009 §3.4).
func TestSignalAdapterIsPairingChannel(t *testing.T) {
	var _ PairingChannel = NewSignalAdapter(msg.ChannelConfig{})
	var _ LinkableChannel = NewSignalAdapter(msg.ChannelConfig{})
}

// TestSignalAutoLinkGuardRefusesWhenAccountExists: the re-link guard (design
// 009 §6). A daemon that already holds a registered account must NOT be
// re-provisioned by an auto-link at Start — the flow degrades to an "error"
// pairing event naming the account, and startLink is never reached (no "qr").
func TestSignalAutoLinkGuardRefusesWhenAccountExists(t *testing.T) {
	a := startLinkAdapterAccounts(t, "sgnl://linkdevice?uuid=G", `{"number":"+1"}`, "",
		`[{"number":"+15550001111"}]`, true)

	ev := nextPairing(t, a.PairingCh())
	if ev.Kind != "error" {
		t.Fatalf("event = %+v, want the re-link-guard error, never a qr", ev)
	}
	if !strings.Contains(ev.Detail, "already linked") || !strings.Contains(ev.Detail, "+15550001111") {
		t.Fatalf("guard error must name the linked account: %q", ev.Detail)
	}
}

// TestSignalTriggerLinkRunsFlowOnDemand: `##humanoid pair link` path — an
// adapter started WITHOUT Link runs the full qr→linked flow when TriggerLink
// is called and the daemon holds no account.
func TestSignalTriggerLinkRunsFlowOnDemand(t *testing.T) {
	const uri = "sgnl://linkdevice?uuid=OD"
	a := startLinkAdapterAccounts(t, uri, `{"number":"+15550002222"}`, "", "", false)

	if err := a.TriggerLink(false); err != nil {
		t.Fatalf("TriggerLink: %v", err)
	}
	if ev := nextPairing(t, a.PairingCh()); ev.Kind != "qr" || ev.QR != uri {
		t.Fatalf("first event = %+v, want qr carrying %q", ev, uri)
	}
	if ev := nextPairing(t, a.PairingCh()); ev.Kind != "linked" {
		t.Fatalf("second event = %+v, want linked", ev)
	}
}

// TestSignalTriggerLinkForceOverridesGuard: force re-links even when the
// daemon already holds an account (the explicit escape hatch the guard
// message points at).
func TestSignalTriggerLinkForceOverridesGuard(t *testing.T) {
	const uri = "sgnl://linkdevice?uuid=F"
	a := startLinkAdapterAccounts(t, uri, `{"number":"+15550003333"}`, "",
		`[{"number":"+15550003333"}]`, false)

	if err := a.TriggerLink(true); err != nil {
		t.Fatalf("TriggerLink(force): %v", err)
	}
	if ev := nextPairing(t, a.PairingCh()); ev.Kind != "qr" || ev.QR != uri {
		t.Fatalf("first event = %+v, want qr (guard must be bypassed by force)", ev)
	}
	if ev := nextPairing(t, a.PairingCh()); ev.Kind != "linked" {
		t.Fatalf("second event = %+v, want linked", ev)
	}
}

// TestSignalTriggerLinkRequiresStart: a never-started adapter refuses
// synchronously instead of hanging on a nil connection.
func TestSignalTriggerLinkRequiresStart(t *testing.T) {
	a := NewSignalAdapter(msg.ChannelConfig{})
	if err := a.TriggerLink(false); err == nil {
		t.Fatal("TriggerLink on an unstarted adapter must error")
	}
}
