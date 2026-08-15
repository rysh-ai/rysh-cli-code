// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// Per-tenant policy (design 022 §4.3).
//
// The problem: every control in the proxy keys off paneID, so capping a
// customer means minting a key per customer. Tenanting lets one upstream key
// carry per-customer ceilings, per-customer rates and per-customer audit.
//
// RESOLUTION ORDER IS A SECURITY PROPERTY, NOT A CONVENIENCE
// ---------------------------------------------------------
//  1. A tenant PINNED to the pane in config.        (authoritative)
//  2. The X-Rysh-Tenant header, only when the operator has set
//     trust_tenant_header AND the pane has no pin.
//  3. The default tenant ("").
//
// Anything running inside a pane can set a request header. If the header could
// override a pin, a pane would simply name a more generous tenant and every cap
// in the system would become advisory. So a pin always wins, and the header is
// not trusted at all unless the operator says otherwise.

// DefaultTenantHeader is the header a caller may use to name its tenant.
const DefaultTenantHeader = "X-Rysh-Tenant"

// ryshHeaderPrefix marks rysh's own control headers. They are stripped before
// forwarding: they are an internal contract, and leaking one to Anthropic tells
// the provider about a customer relationship it has no business knowing.
const ryshHeaderPrefix = "X-Rysh-"

// tenantResolver answers "whose request is this?".
type tenantResolver struct {
	cfg  config.ProxyTenantConfig
	pins map[string]string // paneID → tenant
}

// newTenantResolver returns nil when no tenant is configured, and every call
// site treats nil as "untenanted" — so the feature costs one nil check when off.
func newTenantResolver(cfg config.ProxyTenantConfig) *tenantResolver {
	if len(cfg.Tenants) == 0 {
		return nil
	}
	pins := map[string]string{}
	for name, rule := range cfg.Tenants {
		for _, pane := range rule.Panes {
			if p := strings.TrimSpace(pane); p != "" {
				pins[p] = name
			}
		}
	}
	return &tenantResolver{cfg: cfg, pins: pins}
}

// headerName is the configured tenant header, defaulted.
func (t *tenantResolver) headerName() string {
	if t != nil && strings.TrimSpace(t.cfg.Header) != "" {
		return http.CanonicalHeaderKey(strings.TrimSpace(t.cfg.Header))
	}
	return DefaultTenantHeader
}

// resolve applies the precedence above. An unknown tenant name from a header is
// rejected rather than created: a caller must not be able to invent a tenant
// that has no configured caps and thereby escape every one of them.
func (t *tenantResolver) resolve(paneID string, h http.Header) string {
	if t == nil {
		return ""
	}
	if pinned, ok := t.pins[paneID]; ok {
		return pinned
	}
	if !t.cfg.TrustHeader {
		return ""
	}
	name := strings.TrimSpace(h.Get(t.headerName()))
	if name == "" {
		return ""
	}
	if _, known := t.cfg.Tenants[name]; !known {
		return ""
	}
	return name
}

// rateRule returns the tenant's request-rate rule, if any.
//
// There is deliberately no ceiling() counterpart. Rates are enforced HERE, in
// the proxy's own limiter, so the resolver must answer for them; ceilings are
// enforced by the ledger, which owns the spend they are compared against. The
// proxy only ships the tenant name in MsgUsageCheck and lets the ledger decide.
// Ceilings reach it once, at startup, via the package-level Ceilings().
func (t *tenantResolver) rateRule(tenant string) config.RateLimitRule {
	if t == nil || tenant == "" {
		return config.RateLimitRule{}
	}
	return t.cfg.Tenants[tenant].RateLimit
}

// keyFor returns the upstream credential this tenant's traffic should use for a
// dialect (design 022 §8.3), or "" to fall back to the dialect-wide key.
//
// A tenant that selects the CREDENTIAL and not just the cap is what lets a
// reseller bill each customer on that customer's own provider account — and it
// is a boundary, not a convenience: with per-tenant keys configured, one
// customer's request can never be signed with another's key.
func (t *tenantResolver) keyFor(tenant, dialect string) string {
	if t == nil || tenant == "" {
		return ""
	}
	return strings.TrimSpace(t.cfg.Tenants[tenant].Keys[dialect])
}

// HasTenantKeys reports whether any tenant selects its own credential, for
// ##proxy status. A credential boundary the operator cannot see is one they
// cannot trust.
func HasTenantKeys(cfg config.ProxyTenantConfig) bool {
	for _, rule := range cfg.Tenants {
		for _, k := range rule.Keys {
			if strings.TrimSpace(k) != "" {
				return true
			}
		}
	}
	return false
}

// Ceilings maps every configured tenant to its ceiling, for seeding the ledger.
func Ceilings(cfg config.ProxyTenantConfig) map[string]int64 {
	out := map[string]int64{}
	for name, rule := range cfg.Tenants {
		if rule.CeilingTokens > 0 {
			out[name] = rule.CeilingTokens
		}
	}
	return out
}

// stripRyshHeaders removes rysh's control headers from an outbound request.
//
// copyHeaders forwards everything that is not hop-by-hop, so without this
// X-Rysh-Tenant would travel to the provider verbatim.
func stripRyshHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), ryshHeaderPrefix) {
			h.Del(k)
		}
	}
}
