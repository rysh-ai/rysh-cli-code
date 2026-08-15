// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// Request-rate limiting (design 022 §4.2).
//
// budget.go bounds how many TOKENS a pane may spend; this bounds how many
// REQUESTS it may make. They are different failure modes: a wrapped CLI stuck
// in a retry loop can hammer a provider hundreds of times a second while
// spending almost nothing, and a token ceiling has nothing to say about it.
//
// Token bucket, not the fixed window rysh-server's InvokeRateLimiter uses. A
// fixed window admits a full burst at the end of one window and another at the
// start of the next — a 2x spike precisely when a retrying agent is hammering,
// which is the case this exists to contain. x/time/rate's Limiter also takes an
// explicit `now` on every call, so the injectable clock the tests need comes
// for free rather than being bolted on.
//
// Default is UNLIMITED. A governance control that silently throttles a session
// that used to work is a worse outcome than one that is off until asked for —
// the same posture as [proxy] enabled.

// rateScope names which rule refused a request, for the message and the audit.
type rateScope string

const (
	scopePane    rateScope = "pane"
	scopeTenant  rateScope = "tenant"
	scopeDialect rateScope = "dialect"
	scopeSession rateScope = "session"
)

// rateLimiter holds one token bucket per scope. Buckets are created lazily:
// a pane that never makes a request never costs a bucket.
//
// The mutex guards only the bucket maps — the same documented exception to the
// no-mutex rule as the audit ring and the budget cache, and for the same
// reason: this lives on the HTTP data plane, not inside an actor.
type rateLimiter struct {
	cfg config.ProxyRateLimitConfig

	// tenantRule resolves a tenant's own rate rule (design 022 §4.3). nil when
	// no tenant is configured.
	tenantRule func(tenant string) config.RateLimitRule

	mu       sync.Mutex
	panes    map[string]*rate.Limiter
	tenants  map[string]*rate.Limiter
	dialects map[string]*rate.Limiter
	global   *rate.Limiter

	// now is injectable so tests advance a clock instead of sleeping.
	now func() time.Time
}

// newRateLimiter builds a limiter from config. It returns nil when no rule is
// configured, and every call site treats a nil limiter as "allow" — so the
// disabled path costs one nil check, not a map lookup and a bucket.
//
// tenantRule may be nil; when set, a tenant's own rule binds alongside the rest,
// which is why a limiter is built even with no global rules once tenants exist.
func newRateLimiter(cfg config.ProxyRateLimitConfig, tenantRule func(string) config.RateLimitRule) *rateLimiter {
	if !cfg.Any() && tenantRule == nil {
		return nil
	}
	l := &rateLimiter{
		cfg:        cfg,
		tenantRule: tenantRule,
		panes:      map[string]*rate.Limiter{},
		tenants:    map[string]*rate.Limiter{},
		dialects:   map[string]*rate.Limiter{},
		now:        time.Now,
	}
	if cfg.Global.Enabled() {
		l.global = newBucket(cfg.Global)
	}
	return l
}

func newBucket(r config.RateLimitRule) *rate.Limiter {
	return rate.NewLimiter(rate.Limit(r.Rate), r.EffectiveBurst())
}

// allow reports whether this request may proceed. On refusal it returns how
// long the caller should wait and which rule bound.
//
// Scopes are reserved in order (pane → dialect → session) and any reservation
// already taken is CANCELLED when a later scope refuses. Without that, a
// request rejected by the session cap would still have consumed the pane's
// token, so a pane would be throttled for traffic it never actually sent.
func (l *rateLimiter) allow(paneID, dialect, tenant string) (ok bool, retryAfter time.Duration, scope rateScope) {
	if l == nil {
		return true, 0, ""
	}
	now := l.now()

	var taken []*rate.Reservation
	release := func() {
		for _, r := range taken {
			r.CancelAt(now)
		}
	}

	for _, c := range []struct {
		lim   *rate.Limiter
		scope rateScope
	}{
		{l.paneLimiter(paneID), scopePane},
		{l.tenantLimiter(tenant), scopeTenant},
		{l.dialectLimiter(dialect), scopeDialect},
		{l.global, scopeSession},
	} {
		if c.lim == nil {
			continue
		}
		r := c.lim.ReserveN(now, 1)
		if !r.OK() {
			// Burst smaller than one request: this rule can never admit
			// anything. Report it rather than blocking forever.
			release()
			return false, 0, c.scope
		}
		if d := r.DelayFrom(now); d > 0 {
			// Not available yet. Cancel this reservation too — we are
			// refusing, not queueing, so the token must go back.
			r.CancelAt(now)
			release()
			return false, d, c.scope
		}
		taken = append(taken, r)
	}
	return true, 0, ""
}

func (l *rateLimiter) paneLimiter(paneID string) *rate.Limiter {
	if !l.cfg.PerPane.Enabled() || paneID == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim, ok := l.panes[paneID]; ok {
		return lim
	}
	lim := newBucket(l.cfg.PerPane)
	l.panes[paneID] = lim
	return lim
}

// tenantLimiter bounds one customer's rate across every pane it owns — which is
// the point: a tenant with ten panes must not get ten times the throughput.
func (l *rateLimiter) tenantLimiter(tenant string) *rate.Limiter {
	if l.tenantRule == nil || tenant == "" {
		return nil
	}
	r := l.tenantRule(tenant)
	if !r.Enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim, ok := l.tenants[tenant]; ok {
		return lim
	}
	lim := newBucket(r)
	l.tenants[tenant] = lim
	return lim
}

func (l *rateLimiter) dialectLimiter(dialect string) *rate.Limiter {
	r, ok := l.cfg.PerDialect[dialect]
	if !ok || !r.Enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim, ok := l.dialects[dialect]; ok {
		return lim
	}
	lim := newBucket(r)
	l.dialects[dialect] = lim
	return lim
}

// forget drops a pane's bucket when the pane goes away, so a long-lived daemon
// churning through panes does not accumulate one bucket per pane forever.
func (l *rateLimiter) forget(paneID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.panes, paneID)
	l.mu.Unlock()
}

// rateLimitMessage is the human-readable text in the 429 body. It names the
// rule that bound and the config key that changes it, so the operator can act
// without reading docs — same contract as budgetMessage.
func (l *rateLimiter) message(scope rateScope, dialect, tenant string) string {
	var r config.RateLimitRule
	var what, where string
	switch scope {
	case scopePane:
		r, what, where = l.cfg.PerPane, "this pane", "[proxy] rate_limit"
	case scopeTenant:
		if l.tenantRule != nil {
			r = l.tenantRule(tenant)
		}
		what, where = "tenant "+tenant, "[proxy] tenants"
	case scopeDialect:
		r, what, where = l.cfg.PerDialect[dialect], "the "+dialect+" dialect", "[proxy] rate_limit"
	case scopeSession:
		r, what, where = l.cfg.Global, "this session", "[proxy] rate_limit"
	}
	return fmt.Sprintf(
		"rysh request-rate limit: %s is capped at %s req/s (burst %d). "+
			"Raise or clear it under %s in rysh.config.yaml.",
		what, trimFloat(r.Rate), r.EffectiveBurst(), where)
}

// trimFloat renders a rate without trailing noise: 2 not 2.0, 0.5 not 0.50.
func trimFloat(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}
