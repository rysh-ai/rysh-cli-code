// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"testing"
	"time"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// root-order-044 #8: the workspace USD spend cap, client side.

func capStub(capMicro, spent int64) *upstreamStub {
	// A token ceiling far out of reach, so only the cap binds (the stub reads a
	// ceiling of 0 as "nothing left", not "unlimited").
	stub := newUpstreamStub(1 << 40)
	stub.capMicro, stub.capPeriod, stub.spentUSD = capMicro, sharedmsg.SpendCapPeriodMonth, spent
	return stub
}

// capClient is newClient with a clock read under the stub's lock, because these
// tests move the clock while a renewal runs on another goroutine.
func capClient(t *testing.T, stub *upstreamStub) *LedgerClient {
	t.Helper()
	c := newClient(t, stub, "daemon-a")
	c.now = func() time.Time {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return stub.now
	}
	return c
}

func (s *upstreamStub) usdCalls() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.usdReported...)
}

func TestSpendCap_RefusalNamesTheCap(t *testing.T) {
	stub := capStub(10_000_000, 10_000_000)
	c := capClient(t, stub)
	c.Allow("")
	waitFor(t, "the spend-cap answer", func() bool {
		ls := c.Leases()
		return len(ls) == 1 && ls[0].Exhausted
	})
	ok, verdict := c.Allow("")
	if ok || verdict != VerdictSpendCap {
		t.Fatalf("Allow = %v, %q; want refused with %q", ok, verdict, VerdictSpendCap)
	}
	st, active := c.SpendCap("")
	if !active || st.CapMicroUSD != 10_000_000 || st.SpentMicroUSD != 10_000_000 || st.Period != "month" {
		t.Fatalf("SpendCap = %+v, %v", st, active)
	}
}

// A token-ceiling refusal keeps its own verdict even when a cap is set.
func TestSpendCap_TokenCeilingKeepsItsVerdict(t *testing.T) {
	stub := capStub(10_000_000, 0)
	stub.ceiling, stub.spent = 1, 1
	c := capClient(t, stub)
	c.Allow("")
	waitFor(t, "the ceiling answer", func() bool {
		ls := c.Leases()
		return len(ls) == 1 && ls[0].Exhausted
	})
	if ok, verdict := c.Allow(""); ok || verdict != VerdictExhausted {
		t.Fatalf("Allow = %v, %q; want refused with %q", ok, verdict, VerdictExhausted)
	}
}

func TestSpendCap_NoCapIsInactive(t *testing.T) {
	stub := newUpstreamStub(1_000_000)
	c := capClient(t, stub)
	c.Allow("")
	waitFor(t, "a lease", func() bool { return len(c.Leases()) == 1 && c.Leases()[0].Granted > 0 })
	if st, active := c.SpendCap(""); active {
		t.Fatalf("no cap on the server but SpendCap = %+v", st)
	}
}

// An exhausted scope asks again once per lease TTL, so raising the cap or a
// new period unblocks a running daemon. It keeps refusing until the server
// grants: fail closed.
func TestSpendCap_ExhaustedScopeAsksAgainAfterTTL(t *testing.T) {
	stub := capStub(10_000_000, 10_000_000)
	c := capClient(t, stub)
	c.Allow("")
	waitFor(t, "the spend-cap answer", func() bool {
		ls := c.Leases()
		return len(ls) == 1 && ls[0].Exhausted
	})
	calls := stub.leaseCallCount()

	// Within the TTL: still refused, and no new lease call.
	for i := 0; i < 5; i++ {
		if ok, _ := c.Allow(""); ok {
			t.Fatal("allowed inside the TTL of a spend-cap refusal")
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := stub.leaseCallCount(); got != calls {
		t.Fatalf("lease calls inside the TTL = %d, want %d (no re-ask)", got, calls)
	}

	// The admin raises the cap, and the TTL passes.
	stub.mu.Lock()
	stub.capMicro = 50_000_000
	stub.now = stub.now.Add(sharedmsg.GatewayLeaseTTL + time.Second)
	stub.mu.Unlock()
	if ok, _ := c.Allow(""); ok {
		t.Fatal("allowed before the server answered: must stay refused until a grant")
	}
	waitFor(t, "a grant after the cap was raised", func() bool {
		ls := c.Leases()
		return len(ls) == 1 && !ls[0].Exhausted && ls[0].Granted > 0
	})
	if ok, verdict := c.Allow(""); !ok {
		t.Fatalf("still refused after a grant: %q", verdict)
	}
}

// The daemon reports its USD spend since the last lease call, and does not
// report the same money twice.
func TestSpendCap_ReportsUSDSpendOnce(t *testing.T) {
	stub := capStub(10_000_000, 0)
	// The stub has the first call before the client has its reply (QA flake
	// on 51b623bf): hold that window open so the test cannot race it.
	stub.replyDelay = 150 * time.Millisecond
	c := capClient(t, stub)
	c.Record(msg.MsgUsageRecord{Tenant: "", InTokens: 10, OutTokens: 10, CostMicroUSD: 700, TS: stub.now})
	c.Record(msg.MsgUsageRecord{Tenant: "acme", InTokens: 1, OutTokens: 1, CostMicroUSD: 50, TS: stub.now})
	c.Allow("")
	// Wait for the client to have the first grant, not just for the stub to
	// have the call: until the reply lands the lease is still renewing, and an
	// Allow in that window starts no second renewal (ledger.go needsRenewLocked).
	waitFor(t, "the first grant", func() bool {
		for _, l := range c.Leases() {
			if l.Scope == sharedmsg.TenantScope("") && l.Granted > 0 {
				return len(stub.usdCalls()) == 1
			}
		}
		return false
	})
	if got := stub.usdCalls()[0]; got != 750 {
		t.Fatalf("first lease reported %d micro-USD, want 750 (every tenant)", got)
	}
	// Force a renewal: expire the lease.
	stub.mu.Lock()
	stub.now = stub.now.Add(sharedmsg.GatewayLeaseTTL + time.Second)
	stub.mu.Unlock()
	c.Allow("")
	waitFor(t, "the second lease", func() bool { return len(stub.usdCalls()) == 2 })
	if got := stub.usdCalls()[1]; got != 0 {
		t.Fatalf("second lease reported %d micro-USD, want 0 (already reported)", got)
	}
}
