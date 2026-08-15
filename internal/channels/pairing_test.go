// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// fileStore builds a PairingStore with no NATS connection so every operation
// exercises the .rysh/channel-state file fallback, rooted in a temp dir.
func fileStore(t *testing.T, opts ...PairingOption) *PairingStore {
	t.Helper()
	t.Chdir(t.TempDir())
	return NewPairingStore(nil, opts...)
}

func pendingInbound(sender, name, content string) InboundMessage {
	return InboundMessage{SenderID: sender, SenderName: name, Content: content}
}

// TestPairingLifecycleFileFallback walks the full allow/pending/approve flow
// on the file fallback: unknown sender denied, AddPending issues a code,
// Approve promotes the sender (single-use), and the state survives a
// "restart" (a fresh store over the same working directory).
func TestPairingLifecycleFileFallback(t *testing.T) {
	s := fileStore(t)

	if s.Allowed("support", "whatsapp", "+15551234") {
		t.Fatal("unknown sender must not be allowed (fail-closed)")
	}

	req, err := s.AddPending("support", "whatsapp", pendingInbound("+15551234", "Ada", "hi there"))
	if err != nil {
		t.Fatalf("AddPending: %v", err)
	}
	if len(req.Code) != pairCodeLen {
		t.Fatalf("code length = %d, want %d", len(req.Code), pairCodeLen)
	}
	if req.FirstMsg != "hi there" || req.Channel != "whatsapp" {
		t.Fatalf("unexpected pending req: %+v", req)
	}
	if s.Allowed("support", "whatsapp", "+15551234") {
		t.Fatal("pending sender must still be denied until approved")
	}

	// Unknown code: no-op, ok=false, no error.
	if _, ok, err := s.Approve("support", "whatsapp", "zzzzzz"); ok || err != nil {
		t.Fatalf("unknown code: ok=%v err=%v, want false,nil", ok, err)
	}

	got, ok, err := s.Approve("support", "whatsapp", req.Code)
	if err != nil || !ok {
		t.Fatalf("Approve: ok=%v err=%v", ok, err)
	}
	if got.SenderID != "+15551234" {
		t.Fatalf("approved sender = %q", got.SenderID)
	}
	if !s.Allowed("support", "whatsapp", "+15551234") {
		t.Fatal("approved sender must be allowed")
	}

	// Codes are single-use: a second approve of the same code is a no-op.
	if _, ok, _ := s.Approve("support", "whatsapp", req.Code); ok {
		t.Fatal("consumed code must not approve twice")
	}

	// Allowlist entries are scoped per (humanoid, channel).
	if s.Allowed("support", "slack", "+15551234") || s.Allowed("other", "whatsapp", "+15551234") {
		t.Fatal("allowlist leaked across channel/humanoid boundaries")
	}

	// A fresh store over the same directory sees the persisted allowlist.
	restarted := NewPairingStore(nil)
	if !restarted.Allowed("support", "whatsapp", "+15551234") {
		t.Fatal("allowlist did not survive restart via file fallback")
	}
}

// TestPairingAllowIdempotent verifies direct Allow is additive and duplicate-free.
func TestPairingAllowIdempotent(t *testing.T) {
	s := fileStore(t)
	for i := 0; i < 3; i++ {
		if err := s.Allow("support", "slack", "U123"); err != nil {
			t.Fatalf("Allow #%d: %v", i, err)
		}
	}
	rec, err := s.List("support", "slack")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rec.Allowlist) != 1 || rec.Allowlist[0] != "U123" {
		t.Fatalf("allowlist = %v, want exactly [U123]", rec.Allowlist)
	}
	if err := s.Allow("support", "slack", ""); err == nil {
		t.Fatal("empty sender must be rejected")
	}
}

// TestPairingMaxPendingCap verifies the pending cap refuses over-cap requests
// and that a repeat message from an already-pending sender reuses its request
// instead of eating a slot.
func TestPairingMaxPendingCap(t *testing.T) {
	s := fileStore(t, WithMaxPending(2))

	first, err := s.AddPending("support", "whatsapp", pendingInbound("+1", "A", "one"))
	if err != nil {
		t.Fatalf("AddPending 1: %v", err)
	}
	if _, err := s.AddPending("support", "whatsapp", pendingInbound("+2", "B", "two")); err != nil {
		t.Fatalf("AddPending 2: %v", err)
	}
	if _, err := s.AddPending("support", "whatsapp", pendingInbound("+3", "C", "three")); err == nil {
		t.Fatal("third pending must be refused at max_pending=2")
	}

	// Same sender again: dedupe returns the existing request (same code).
	again, err := s.AddPending("support", "whatsapp", pendingInbound("+1", "A", "one more"))
	if err != nil {
		t.Fatalf("dedupe AddPending: %v", err)
	}
	if again.Code != first.Code {
		t.Fatalf("dedupe minted a new code: %q vs %q", again.Code, first.Code)
	}
}

// TestPairingTTLSweep verifies expired pendings are swept and an expired code
// no longer approves.
func TestPairingTTLSweep(t *testing.T) {
	// Fake clock via WithNow: the store is only touched from this goroutine,
	// and advancing time deterministically is the point — the previous
	// real-sleep version (20ms TTL vs 40ms sleep) flaked under -race on CI
	// when >20ms elapsed between the second AddPending and List, expiring
	// the entry this test asserts survives.
	clock := time.Now()
	s := fileStore(t, WithPendingTTL(20*time.Millisecond), WithMaxPending(1),
		WithNow(func() time.Time { return clock }))

	req, err := s.AddPending("support", "signal", pendingInbound("+9", "Z", "hello"))
	if err != nil {
		t.Fatalf("AddPending: %v", err)
	}
	clock = clock.Add(40 * time.Millisecond) // first request now past its TTL

	// The expired slot is reclaimed: a new sender fits despite max_pending=1.
	if _, err := s.AddPending("support", "signal", pendingInbound("+8", "Y", "hey")); err != nil {
		t.Fatalf("expired slot was not reclaimed: %v", err)
	}

	// The expired code is dead.
	if _, ok, _ := s.Approve("support", "signal", req.Code); ok {
		t.Fatal("expired code must not approve")
	}
	rec, _ := s.List("support", "signal")
	if len(rec.Pending) != 1 {
		t.Fatalf("List after sweep: %d pending, want 1", len(rec.Pending))
	}
	if s.Allowed("support", "signal", "+9") {
		t.Fatal("expired requester must not be allowed")
	}
}

// TestNewPairCodeCharset checks length, charset, and practical uniqueness.
func TestNewPairCodeCharset(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		code := newPairCode()
		if len(code) != pairCodeLen {
			t.Fatalf("code %q length %d, want %d", code, len(code), pairCodeLen)
		}
		for _, r := range code {
			if !strings.ContainsRune(shortIDAlphabet, r) {
				t.Fatalf("code %q contains %q outside base36 alphabet", code, r)
			}
		}
		seen[code] = true
	}
	// 500 draws from 36^6 should essentially never collide; a heavy collision
	// rate indicates a broken generator.
	if len(seen) < 495 {
		t.Fatalf("suspicious collision rate: %d unique of 500", len(seen))
	}
}

// TestDeviceLinkRoundTrip verifies device-link blobs persist, reload across a
// restart, and are blanked out of List (secrets never feed rendering paths).
func TestDeviceLinkRoundTrip(t *testing.T) {
	s := fileStore(t)
	blob := json.RawMessage(`{"session":"opaque-creds"}`)

	if _, ok := s.LoadDeviceLink("support", "whatsapp"); ok {
		t.Fatal("no device link expected before save")
	}
	if err := s.SaveDeviceLink("support", "whatsapp", blob); err != nil {
		t.Fatalf("SaveDeviceLink: %v", err)
	}
	got, ok := s.LoadDeviceLink("support", "whatsapp")
	if !ok || !bytes.Equal(got, blob) {
		t.Fatalf("LoadDeviceLink = %s ok=%v", got, ok)
	}

	// The blob must survive a restart and coexist with allowlist writes.
	if err := s.Allow("support", "whatsapp", "+1"); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	restarted := NewPairingStore(nil)
	got, ok = restarted.LoadDeviceLink("support", "whatsapp")
	if !ok || !bytes.Equal(got, blob) {
		t.Fatalf("device link lost on restart: %s ok=%v", got, ok)
	}

	// List blanks the secret.
	rec, err := restarted.List("support", "whatsapp")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if rec.DeviceLink != nil {
		t.Fatal("List must not expose the device-link blob")
	}
}

// startJetStreamNATS spins up an in-process NATS server with JetStream enabled
// (mirroring how the embedded bus provides KV to the daemon) and returns a
// connected client.
func startJetStreamNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{
		Host:       "127.0.0.1",
		Port:       -1,
		NoLog:      true,
		NoSigs:     true,
		DontListen: true,
		JetStream:  true,
		StoreDir:   t.TempDir(),
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Skipf("embedded NATS unavailable: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Skip("nats server not ready")
	}
	t.Cleanup(ns.Shutdown)

	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		ns.Shutdown()
		t.Fatalf("connect in-process nats: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// TestPairingKVBacked exercises the JetStream-KV path end to end: the bucket
// is created, records round-trip, and a second store over the same server
// (a "restarted daemon") sees the persisted state without touching files.
func TestPairingKVBacked(t *testing.T) {
	t.Chdir(t.TempDir()) // any accidental file fallback stays sandboxed
	nc := startJetStreamNATS(t)

	s := NewPairingStore(nc)
	if s.kv == nil {
		t.Fatal("expected KV-backed store on a JetStream-enabled server")
	}

	req, err := s.AddPending("support", "whatsapp", pendingInbound("+7", "Kay", "hello via kv"))
	if err != nil {
		t.Fatalf("AddPending: %v", err)
	}
	if _, ok, err := s.Approve("support", "whatsapp", req.Code); !ok || err != nil {
		t.Fatalf("Approve: ok=%v err=%v", ok, err)
	}
	if err := s.SaveDeviceLink("support", "whatsapp", json.RawMessage(`{"kv":true}`)); err != nil {
		t.Fatalf("SaveDeviceLink: %v", err)
	}

	// A second store over the same server binds the existing bucket.
	s2 := NewPairingStore(nc)
	if !s2.Allowed("support", "whatsapp", "+7") {
		t.Fatal("allowlist not visible from a second KV-backed store")
	}
	if blob, ok := s2.LoadDeviceLink("support", "whatsapp"); !ok || !bytes.Equal(blob, json.RawMessage(`{"kv":true}`)) {
		t.Fatalf("device link not visible from second store: %s ok=%v", blob, ok)
	}
}
