// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Budget enforcement (design 001 §4.4).
//
// Before forwarding, the proxy asks the usage ledger whether the pane is still
// under its token ceiling. Over ceiling ⇒ a dialect-shaped 429, so the wrapped
// CLI surfaces a normal rate-limit error and stops cleanly instead of running
// up a bill.
//
// The ledger answers on a request/reply subject served by UsageActor, whose
// mailbox is sequential — so the reply is cached per pane (2 s, per the design)
// to keep a per-HTTP-request round trip off the actor's hot path.

// budgetCheckTimeout bounds the ledger round trip. The UsageActor is in-process,
// so this only ever fires when something is badly wrong.
const budgetCheckTimeout = 250 * time.Millisecond

// budgetCacheTTL is the per-pane reply cache window (design 001 §4.4).
const budgetCacheTTL = 2 * time.Second

// budgetRetryAfter is what a ceiling refusal advertises in Retry-After. The
// ceiling is a DAILY figure, so there is no exact answer here — an hour is
// deliberately coarse but honest, and it is the value this path has always
// sent. The request-rate limiter, which does know precisely when the next
// token frees, supplies its own (design 022 §4.2).
const budgetRetryAfter = time.Hour

type budgetEntry struct {
	reply   msg.MsgUsageCheckReply
	fetched time.Time
}

// budgetChecker caches ledger replies per pane.
//
// The mutex guards only this small map — the same documented exception to the
// no-mutex rule as the audit ring, and for the same reason: it lives on the
// HTTP data plane, not inside an actor.
type budgetChecker struct {
	// request is the ledger round trip, injectable so tests need no broker.
	// nil ⇒ no ledger configured ⇒ everything is allowed.
	request func(subject string, m interface{}, timeout time.Duration) (interface{}, error)

	mu    sync.Mutex
	cache map[string]budgetEntry
	// now is injectable so tests can advance the clock without sleeping.
	now func() time.Time
}

func newBudgetChecker(pub *msg.NATSPublisher) *budgetChecker {
	b := &budgetChecker{
		cache: make(map[string]budgetEntry),
		now:   time.Now,
	}
	if pub != nil {
		b.request = pub.Request
	}
	return b
}

// check reports whether the pane may spend, along with the ledger's figures.
//
// Failure mode is deliberate and documented: when the ledger cannot be reached
// the request is ALLOWED, with an error log. Budget enforcement is a cost
// control, not a security boundary — a transient ledger blip must not take down
// every agent's provider traffic, and the boundary that does protect the user
// (SNAT redaction) is applied independently and is unaffected. A ceiling breach
// is therefore best-effort by design; `##cost` remains the source of truth.
// tenant, when non-empty, makes the ledger check that customer's ceiling too;
// the stricter of the two binds. It is part of the cache key, because two panes
// under different tenants must not share a verdict.
func (b *budgetChecker) check(paneID, tenant string) (allowed bool, reply msg.MsgUsageCheckReply) {
	if b == nil || b.request == nil || paneID == "" {
		return true, msg.MsgUsageCheckReply{PaneID: paneID, Ok: true}
	}

	key := paneID
	if tenant != "" {
		key = paneID + "\x00" + tenant
	}
	if e, ok := b.cached(key); ok {
		return e.Ok, e
	}

	raw, err := b.request(msg.UsageCheckSubject(),
		&msg.MsgUsageCheck{PaneID: paneID, Tenant: tenant}, budgetCheckTimeout)
	if err != nil {
		slog.Error("proxy: budget check failed — allowing request (fail-open, cost control only)",
			"pane", paneID, "err", err)
		return true, msg.MsgUsageCheckReply{PaneID: paneID, Ok: true}
	}

	r, ok := raw.(*msg.MsgUsageCheckReply)
	if !ok || r == nil {
		slog.Error("proxy: budget check returned an unexpected reply — allowing request",
			"pane", paneID, "type", fmt.Sprintf("%T", raw))
		return true, msg.MsgUsageCheckReply{PaneID: paneID, Ok: true}
	}

	b.store(key, *r)
	return r.Ok, *r
}

func (b *budgetChecker) cached(paneID string) (msg.MsgUsageCheckReply, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.cache[paneID]
	if !ok || b.now().Sub(e.fetched) > budgetCacheTTL {
		return msg.MsgUsageCheckReply{}, false
	}
	return e.reply, true
}

func (b *budgetChecker) store(paneID string, r msg.MsgUsageCheckReply) {
	b.mu.Lock()
	b.cache[paneID] = budgetEntry{reply: r, fetched: b.now()}
	b.mu.Unlock()
}

// invalidate drops a pane's cached reply. Called after a request meters usage
// so a breach is noticed on the next request rather than up to a TTL later.
func (b *budgetChecker) invalidate(paneID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	// Cache keys are paneID, optionally suffixed with the tenant. Dropping only
	// the bare key would leave a tenanted pane's verdict stale for the full TTL
	// after it just spent — exactly the case the invalidation exists for.
	delete(b.cache, paneID)
	for k := range b.cache {
		if strings.HasPrefix(k, paneID+"\x00") {
			delete(b.cache, k)
		}
	}
	b.mu.Unlock()
}

// LeaseGate is the seam design 023 §4.2 hangs the org-wide ceiling on.
//
// The contract is the whole point: both methods are IN-PROCESS, non-blocking
// reads/writes of local numbers. The daemon leases a slice of the workspace or
// tenant allowance out of band and enforces against that slice here, so the hot
// path never makes a network call — a per-request WAN round trip would put
// provider-latency-scale delay in front of every request and make a blip look
// like a provider outage.
//
// nil ⇒ no org-wide governance, which is the default and the OSS behaviour.
type LeaseGate interface {
	// Allow reports whether the tenant may spend, and a short machine-readable
	// reason it may not.
	Allow(tenant string) (bool, string)
	// Spend records tokens against the current lease after a request meters.
	Spend(tenant string, tokens int64)
}

// leaseMessage is the 429 body for an org-wide refusal. It deliberately reads
// like the local ceiling's message — a wrapped CLI must not be able to tell the
// two apart (023 §4.2) — while still naming which ceiling bound, because the
// operator's next command differs.
func leaseMessage(tenant string) string {
	who := "this workspace"
	if tenant != "" {
		who = "tenant " + tenant
	}
	return "rysh budget exceeded for " + who + " across all machines: the " +
		"workspace-wide token ceiling is used up for today. Raise it in the rysh " +
		"dashboard, or inspect local spend with `##cost`."
}

// budgetMessage is the human-readable text carried in the 429 body. It names
// the ceiling and the command that changes it, so the operator can act without
// reading docs.
func budgetMessage(r msg.MsgUsageCheckReply) string {
	// Name the ceiling that actually bound. Telling an operator their PANE is
	// over budget when the TENANT's cap is what refused them points at the wrong
	// command and the wrong number.
	if r.Scope == msg.UsageScopeTenant {
		return "rysh budget exceeded for tenant " + r.Tenant + ": " +
			formatTokens(r.SpentTokens) + " of " + formatTokens(r.CeilingTokens) +
			" tokens used today. Raise or clear it under [proxy] tenants in " +
			"rysh.config.yaml, or inspect spend with `##cost`."
	}
	return "rysh budget exceeded for this pane: " +
		formatTokens(r.SpentTokens) + " of " + formatTokens(r.CeilingTokens) +
		" tokens used today. Raise or clear it with `##cost budget <tokens>`, " +
		"or inspect spend with `##cost`."
}

func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}
