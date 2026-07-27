package proxy

import (
	"fmt"
	"log/slog"
	"strconv"
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
func (b *budgetChecker) check(paneID string) (allowed bool, reply msg.MsgUsageCheckReply) {
	if b == nil || b.request == nil || paneID == "" {
		return true, msg.MsgUsageCheckReply{PaneID: paneID, Ok: true}
	}

	if e, ok := b.cached(paneID); ok {
		return e.Ok, e
	}

	raw, err := b.request(msg.UsageCheckSubject(),
		&msg.MsgUsageCheck{PaneID: paneID}, budgetCheckTimeout)
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

	b.store(paneID, *r)
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
	delete(b.cache, paneID)
	b.mu.Unlock()
}

// budgetMessage is the human-readable text carried in the 429 body. It names
// the ceiling and the command that changes it, so the operator can act without
// reading docs.
func budgetMessage(r msg.MsgUsageCheckReply) string {
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
