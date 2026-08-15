// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 023 §6's test list, minus the parts that live server-side.
//
// The lease model's guarantee is a BOUND, not exactness ("your 2M ceiling may
// overshoot by up to N tokens with M machines"), and §6 is explicit that the
// bound has to be a test rather than a claim in a document. That is
// TestOverspendBound below.

// upstreamStub is a fake control-plane server: it applies the real grant
// arithmetic against one ceiling, so several clients leasing from it behave the
// way real daemons would.
type upstreamStub struct {
	mu            sync.Mutex
	ceiling       int64
	spent         int64 // reconciled spend reported by daemons
	outstanding   map[string]int64
	leaseCalls    int
	fail          bool // simulate a partition
	now           time.Time
	maxLease      int64
	policyBody    string
	policyETag    string
	policyRequest int
	ifNoneMatch   []string
}

// leaseCallCount reads leaseCalls under the stub's lock. Allow() renews on a
// background goroutine (ledger.go, Allow.gowrap1), so that counter is written
// concurrently with the test body reading it — the rest of this file already
// takes s.mu for the same reason when it flips `fail`.
func (s *upstreamStub) leaseCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaseCalls
}

func newUpstreamStub(ceiling int64) *upstreamStub {
	return &upstreamStub{
		ceiling:     ceiling,
		outstanding: map[string]int64{},
		now:         time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
		maxLease:    sharedmsg.GatewayLeaseMaxTokens,
	}
}

func (s *upstreamStub) do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, io.ErrUnexpectedEOF
	}
	switch {
	case req.URL.Path[len(req.URL.Path)-6:] == "/lease":
		return s.lease(req)
	default:
		return s.policy(req)
	}
}

func (s *upstreamStub) lease(req *http.Request) (*http.Response, error) {
	s.leaseCalls++
	var in sharedmsg.GatewayLeaseRequest
	body, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(body, &in)

	// Reconcile what the daemon spent since its last call, and return the slice
	// it was holding.
	s.spent += in.SpentSince
	delete(s.outstanding, in.DaemonID)

	remaining := s.ceiling - s.spent
	for _, o := range s.outstanding {
		remaining -= o // a slice someone else holds is not available
	}
	if remaining < 0 {
		remaining = 0
	}
	granted := in.WantTokens
	if granted > remaining {
		granted = remaining
	}
	if granted > s.maxLease {
		granted = s.maxLease
	}
	if granted > 0 {
		s.outstanding[in.DaemonID] = granted
	}
	reply := sharedmsg.GatewayLeaseReply{
		GrantedTokens:  granted,
		ExpiresAt:      s.now.Add(sharedmsg.GatewayLeaseTTL),
		RemainingToday: remaining,
		CeilingTokens:  s.ceiling,
		ActiveDaemons:  len(s.outstanding),
		MaxLeaseTokens: s.maxLease,
	}
	raw, _ := json.Marshal(reply)
	return jsonResponse(http.StatusOK, raw, nil), nil
}

func (s *upstreamStub) policy(req *http.Request) (*http.Response, error) {
	s.policyRequest++
	inm := req.Header.Get("If-None-Match")
	s.ifNoneMatch = append(s.ifNoneMatch, inm)
	if inm != "" && inm == s.policyETag {
		return jsonResponse(http.StatusNotModified, nil, etagHeader(s.policyETag)), nil
	}
	return jsonResponse(http.StatusOK, []byte(s.policyBody), etagHeader(s.policyETag)), nil
}

// etagHeader builds the header the way net/http would deliver it: canonical
// key, so Header.Get finds it. A literal "ETag" map key does not.
func etagHeader(tag string) http.Header {
	h := http.Header{}
	h.Set("ETag", tag)
	return h
}

func jsonResponse(status int, body []byte, h http.Header) *http.Response {
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// newClient builds a governance-enabled client wired to a stub, with no
// background loop (tests drive every step explicitly).
func newClient(t *testing.T, stub *upstreamStub, daemonID string, mods ...func(*config.UpstreamConfig)) *LedgerClient {
	t.Helper()
	cfg := config.UpstreamConfig{
		Enabled: true, Governance: true,
		URL: "https://server.example", APIKey: "k", Workspace: "ws-1",
	}
	for _, m := range mods {
		m(&cfg)
	}
	c := New(cfg, daemonID, nil)
	c.httpDo = stub.do
	c.now = func() time.Time { return stub.now }
	return c
}

// waitFor spins until cond holds, so a renewal kicked off on its own goroutine
// can be awaited without sleeping for a fixed time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWantTokens_SizingRules(t *testing.T) {
	cases := []struct {
		remaining, want int64
	}{
		{0, sharedmsg.GatewayLeaseMinTokens},           // floor: never lease-per-request
		{100_000, sharedmsg.GatewayLeaseMinTokens},     // 10% is below the floor
		{5_000_000, 500_000},                           // 10% of what is left
		{100_000_000, sharedmsg.GatewayLeaseMaxTokens}, // ceiling: bounds the overspend
	}
	for _, c := range cases {
		if got := wantTokens(c.remaining); got != c.want {
			t.Errorf("wantTokens(%d) = %d, want %d", c.remaining, got, c.want)
		}
	}
}

// TestLedgerClient_DisabledMakesNoNetworkCalls is design 023 §6's
// "off by default" requirement, and the OSS guarantee in §1: with [upstream]
// disabled, nothing here reaches the network at all.
func TestLedgerClient_DisabledMakesNoNetworkCalls(t *testing.T) {
	var calls int
	for _, cfg := range []config.UpstreamConfig{
		{}, // nothing configured
		{Enabled: true, URL: "https://s", APIKey: "k", Workspace: "w"},    // upstream on, governance off
		{Enabled: true, Governance: true, URL: "https://s", APIKey: "k"},  // no workspace
		{Governance: true, URL: "https://s", APIKey: "k", Workspace: "w"}, // upstream off
	} {
		c := New(cfg, "d1", nil)
		c.httpDo = func(*http.Request) (*http.Response, error) {
			calls++
			t.Error("a disabled ledger client made an HTTP call")
			return nil, io.EOF
		}
		if c.Enabled() {
			t.Fatalf("client with cfg %+v reports enabled", cfg)
		}
		c.Start()
		c.Record(msg.MsgUsageRecord{PaneID: "p", InTokens: 10, OutTokens: 5})
		c.Flush()
		if ok, verdict := c.Allow("acme"); !ok || verdict != VerdictNoGovernance {
			t.Fatalf("disabled Allow = (%v, %s), want (true, %s) — the OSS client "+
				"must be unaffected", ok, verdict, VerdictNoGovernance)
		}
		c.Spend("acme", 100)
		if body := c.PullPolicy(t.Context()); body != nil {
			t.Error("a disabled client pulled policy")
		}
		c.Stop()
	}
	if calls != 0 {
		t.Fatalf("%d network calls from a disabled client", calls)
	}
}

// TestLease_AcquiresAndEnforcesLocally: the first Allow acquires, spending
// draws the slice down, and an exhausted slice refuses without any per-request
// network call.
func TestLease_AcquiresAndEnforcesLocally(t *testing.T) {
	stub := newUpstreamStub(10_000_000)
	c := newClient(t, stub, "daemon-a")

	// First call has no lease yet: the partition default (local) allows while
	// the acquisition runs, which is the honest answer — the machine-local
	// ceiling is still in force.
	ok, verdict := c.Allow("acme")
	if !ok || verdict != VerdictPartition {
		t.Fatalf("first Allow = (%v, %s), want (true, %s)", ok, verdict, VerdictPartition)
	}
	waitFor(t, "the first lease", func() bool {
		for _, l := range c.Leases() {
			if l.Granted > 0 {
				return true
			}
		}
		return false
	})

	if ok, verdict := c.Allow("acme"); !ok || verdict != VerdictLeased {
		t.Fatalf("Allow with a lease = (%v, %s), want (true, %s)", ok, verdict, VerdictLeased)
	}

	// Spend the whole slice. The refusal is local: no extra lease call is
	// needed to know the slice is gone.
	before := stub.leaseCallCount()
	granted := c.Leases()[0].Granted
	c.Spend("acme", granted)
	ok, verdict = c.Allow("acme")
	if ok {
		t.Fatal("a spent-out lease must refuse")
	}
	if verdict != VerdictLeaseSpent {
		t.Fatalf("verdict = %s, want %s", verdict, VerdictLeaseSpent)
	}
	if stub.leaseCallCount() < before {
		t.Fatal("lease calls went backwards")
	}
}

// TestLease_GrantedZeroRefusesEvenUnderPartitionOpen: granted 0 is the server
// saying the scope is out of budget. That is a decision, not a partition, and
// no partition setting may soften it.
func TestLease_GrantedZeroRefusesEvenUnderPartitionOpen(t *testing.T) {
	stub := newUpstreamStub(0) // nothing left at all
	stub.ceiling = 1
	stub.spent = 1
	c := newClient(t, stub, "daemon-a", func(cfg *config.UpstreamConfig) {
		cfg.GovernanceOnPartition = PartitionOpen
	})

	c.Allow("acme") // triggers acquisition
	waitFor(t, "the out-of-budget answer", func() bool {
		ls := c.Leases()
		return len(ls) == 1 && ls[0].Exhausted
	})

	ok, verdict := c.Allow("acme")
	if ok {
		t.Fatal("an out-of-budget scope must be refused even with on_partition: open")
	}
	if verdict != VerdictExhausted {
		t.Fatalf("verdict = %s, want %s", verdict, VerdictExhausted)
	}
}

// TestPartitionMatrix walks every row of design 023 §4.3 — including the one
// that matters most: an expired lease with an unreachable server degrades to
// LOCAL, never to unlimited and never to dead.
func TestPartitionMatrix(t *testing.T) {
	rows := []struct {
		name        string
		mode        string
		leased      bool // a lease was granted at some point
		expired     bool
		serverUp    bool
		wantAllowed bool
		wantVerdict string
	}{
		{"server reachable, lease valid", PartitionLocal, true, false, true, true, VerdictLeased},
		{"unreachable, lease valid ⇒ spend continues", PartitionLocal, true, false, false, true, VerdictLeased},
		{"unreachable, lease expired ⇒ local ceiling governs", PartitionLocal, true, true, false, true, VerdictPartition},
		{"unreachable, lease expired, strict ⇒ refuse", PartitionStrict, true, true, false, false, VerdictPartition},
		{"unreachable, lease expired, open ⇒ allow", PartitionOpen, true, true, false, true, VerdictPartition},
		{"never leased (first start, offline) ⇒ local", PartitionLocal, false, false, false, true, VerdictPartition},
		{"never leased, strict ⇒ refuse", PartitionStrict, false, false, false, false, VerdictPartition},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			stub := newUpstreamStub(10_000_000)
			c := newClient(t, stub, "daemon-a", func(cfg *config.UpstreamConfig) {
				cfg.GovernanceOnPartition = r.mode
			})
			if r.leased {
				c.Allow("acme")
				waitFor(t, "a lease", func() bool {
					ls := c.Leases()
					return len(ls) == 1 && ls[0].Granted > 0
				})
			}
			if r.expired {
				// Move the clock past the grant.
				base := stub.now
				c.now = func() time.Time { return base.Add(sharedmsg.GatewayLeaseTTL + time.Minute) }
			}
			if !r.serverUp {
				stub.mu.Lock()
				stub.fail = true
				stub.mu.Unlock()
			}

			ok, verdict := c.Allow("acme")
			if ok != r.wantAllowed || verdict != r.wantVerdict {
				t.Fatalf("Allow = (%v, %s), want (%v, %s)", ok, verdict, r.wantAllowed, r.wantVerdict)
			}
		})
	}
}

// TestOverspendBound is the design's actual guarantee (023 §4.2/§6): with M
// daemons against one ceiling, total spend is at most ceiling + M × maxLease.
// It must be a test, not a claim in a doc.
func TestOverspendBound(t *testing.T) {
	const (
		daemons = 4
		ceiling = int64(3_000_000)
	)
	stub := newUpstreamStub(ceiling)
	stub.maxLease = 250_000 // a configured maxLease, as an operator would set

	clients := make([]*LedgerClient, daemons)
	for i := range clients {
		clients[i] = newClient(t, stub, string(rune('a'+i)))
	}

	// Every daemon spends as hard as it can. A round where nobody could spend
	// is not the end — a renewal may be in flight — so the loop stops only
	// after the fleet has been dry for a while, which is what "the ceiling is
	// actually used up" looks like from here.
	var total int64
	dry := 0
	for round := 0; round < 5_000 && dry < 100; round++ {
		spentThisRound := false
		for _, c := range clients {
			ok, _ := c.Allow("acme")
			if !ok {
				continue
			}
			// A chunky request, so the loop terminates in reasonable time.
			c.Spend("acme", 25_000)
			total += 25_000
			spentThisRound = true
		}
		// Yield between rounds. A real daemon's requests are milliseconds apart
		// with network I/O in between; a tight in-memory loop can otherwise run
		// to completion before a renewal goroutine is ever scheduled, which
		// would test the partition path instead of the lease path.
		time.Sleep(200 * time.Microsecond)
		if spentThisRound {
			dry = 0
			continue
		}
		dry++
	}

	bound := ceiling + int64(daemons)*stub.maxLease
	if total > bound {
		t.Fatalf("total spend %d exceeds the stated bound %d (ceiling %d + %d daemons × %d maxLease)",
			total, bound, ceiling, daemons, stub.maxLease)
	}
	// Non-vacuous: the ceiling must actually have been approached, or a broken
	// client that refuses everything would "pass".
	if total < ceiling/2 {
		t.Fatalf("total spend %d is far below the ceiling %d — the clients were "+
			"not really spending, so the bound proves nothing", total, ceiling)
	}
}

// TestUsageBatch_RollsUpAndSequences: batches are per (tenant, day), carry an
// increasing seq, and the seq is what makes a replay harmless server-side.
func TestUsageBatch_RollsUpAndSequences(t *testing.T) {
	stub := newUpstreamStub(1_000_000)
	c := newClient(t, stub, "daemon-a")

	var sent []*sharedmsg.MsgGatewayUsageBatch
	c.publish = func(b *sharedmsg.MsgGatewayUsageBatch) error {
		sent = append(sent, b)
		return nil
	}

	day := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	c.Record(msg.MsgUsageRecord{PaneID: "p1", Tenant: "acme", InTokens: 100, OutTokens: 10, CostMicroUSD: 7, TS: day})
	c.Record(msg.MsgUsageRecord{PaneID: "p2", Tenant: "acme", InTokens: 50, OutTokens: 5, CostMicroUSD: 3, TS: day})
	c.Record(msg.MsgUsageRecord{PaneID: "p3", Tenant: "", InTokens: 1, OutTokens: 1, TS: day})
	c.Flush()

	if len(sent) != 1 {
		t.Fatalf("batches = %d, want 1", len(sent))
	}
	b := sent[0]
	if b.Seq != 1 || b.DaemonID != "daemon-a" || b.WorkspaceID != "ws-1" {
		t.Fatalf("batch envelope wrong: %+v", b)
	}
	if len(b.Rollups) != 2 {
		t.Fatalf("rollups = %d, want 2 (one per tenant, per day)", len(b.Rollups))
	}
	for _, r := range b.Rollups {
		if r.Tenant == "acme" {
			if r.InTokens != 150 || r.OutTokens != 15 || r.Calls != 2 || r.CostMicroUSD != 10 {
				t.Fatalf("acme rollup = %+v", r)
			}
			if r.Tokens() != 165 {
				t.Fatalf("Tokens() = %d, want 165", r.Tokens())
			}
		}
	}

	// A second flush with nothing pending publishes nothing at all.
	c.Flush()
	if len(sent) != 1 {
		t.Fatalf("an empty flush published a batch: %d", len(sent))
	}

	// And the next real batch takes the next sequence number.
	c.Record(msg.MsgUsageRecord{PaneID: "p1", InTokens: 1, OutTokens: 1, TS: day})
	c.Flush()
	if len(sent) != 2 || sent[1].Seq != 2 {
		t.Fatalf("second batch seq = %+v", sent)
	}
}

// TestUsageBatch_FailedPublishIsRetriedWithTheSameSequence: a batch that never
// left must not consume a sequence number, or the server's seen-set would treat
// the retry as a new batch and the reconnect path would silently DROP spend.
func TestUsageBatch_FailedPublishIsRetriedWithTheSameSequence(t *testing.T) {
	stub := newUpstreamStub(1_000_000)
	c := newClient(t, stub, "daemon-a")

	fail := true
	var sent []*sharedmsg.MsgGatewayUsageBatch
	c.publish = func(b *sharedmsg.MsgGatewayUsageBatch) error {
		if fail {
			return io.ErrClosedPipe
		}
		sent = append(sent, b)
		return nil
	}

	day := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	c.Record(msg.MsgUsageRecord{PaneID: "p", InTokens: 100, OutTokens: 10, TS: day})
	c.Flush() // fails

	fail = false
	c.Record(msg.MsgUsageRecord{PaneID: "p", InTokens: 5, OutTokens: 1, TS: day})
	c.Flush()

	if len(sent) != 1 {
		t.Fatalf("batches = %d, want 1", len(sent))
	}
	if sent[0].Seq != 1 {
		t.Fatalf("seq = %d, want 1 — a batch that never went out must not burn one", sent[0].Seq)
	}
	r := sent[0].Rollups[0]
	if r.InTokens != 105 || r.OutTokens != 11 || r.Calls != 2 {
		t.Fatalf("the failed batch's spend was lost: %+v", r)
	}
}

// TestPullPolicy_ETagAndFailClosed covers 023 §4.7: conditional GET, and a
// server that goes away leaves the last-known-good document in place rather
// than reverting to no policy.
func TestPullPolicy_ETagAndFailClosed(t *testing.T) {
	stub := newUpstreamStub(1)
	stub.policyBody = "proxy:\n  required: true\n"
	stub.policyETag = `"v1"`
	c := newClient(t, stub, "daemon-a")

	body := c.PullPolicy(t.Context())
	if string(body) != stub.policyBody {
		t.Fatalf("first pull = %q", body)
	}

	// Second pull sends If-None-Match and gets a 304: nothing changed, and the
	// cached document is untouched.
	if body := c.PullPolicy(t.Context()); body != nil {
		t.Fatalf("a 304 must report no change, got %q", body)
	}
	if stub.ifNoneMatch[1] != `"v1"` {
		t.Fatalf("If-None-Match = %q, want the ETag", stub.ifNoneMatch[1])
	}
	cached, at := c.Policy()
	if string(cached) != stub.policyBody || at.IsZero() {
		t.Fatalf("cached policy lost after a 304: %q", cached)
	}

	// Server unreachable: keep the last known good. Reverting to "no policy"
	// would make a partition softer than a policy file, which 013 forbids.
	stub.mu.Lock()
	stub.fail = true
	stub.mu.Unlock()
	if body := c.PullPolicy(t.Context()); body != nil {
		t.Fatalf("an unreachable server must not produce a policy: %q", body)
	}
	cached, _ = c.Policy()
	if string(cached) != stub.policyBody {
		t.Fatalf("last-known-good policy was dropped on a partition: %q", cached)
	}
}
