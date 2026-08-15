// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func tenantCfg(trust bool) config.ProxyTenantConfig {
	return config.ProxyTenantConfig{
		TrustHeader: trust,
		Tenants: map[string]config.TenantRule{
			"acme": {
				Panes:         []string{"pane-acme"},
				CeilingTokens: 1_000_000,
				RateLimit:     config.RateLimitRule{Rate: 1, Burst: 1},
			},
			"globex": {CeilingTokens: 50},
		},
	}
}

// TestTenant_PinBeatsHostileHeader is THE security property (design 022 §4.3).
// Anything running inside a pane can set a header. If the header could override
// a pin, a pane would name a more generous tenant and every cap in the system
// would become advisory.
func TestTenant_PinBeatsHostileHeader(t *testing.T) {
	// Even with the header TRUSTED, a pin must win.
	r := newTenantResolver(tenantCfg(true))

	h := http.Header{}
	h.Set(DefaultTenantHeader, "globex")

	if got := r.resolve("pane-acme", h); got != "acme" {
		t.Fatalf("resolve = %q, want the PINNED tenant acme — a pane must not be "+
			"able to name a different tenant for itself", got)
	}
}

func TestTenant_HeaderIgnoredUnlessTrusted(t *testing.T) {
	h := http.Header{}
	h.Set(DefaultTenantHeader, "acme")

	untrusted := newTenantResolver(tenantCfg(false))
	if got := untrusted.resolve("pane-other", h); got != "" {
		t.Errorf("resolve = %q, want \"\" — the header is untrusted by default", got)
	}

	trusted := newTenantResolver(tenantCfg(true))
	if got := trusted.resolve("pane-other", h); got != "acme" {
		t.Errorf("resolve = %q, want acme once the operator trusts the header", got)
	}
}

// TestTenant_UnknownNameIsRejected: a caller must not be able to invent a
// tenant that has no configured caps and thereby escape all of them.
func TestTenant_UnknownNameIsRejected(t *testing.T) {
	r := newTenantResolver(tenantCfg(true))
	h := http.Header{}
	h.Set(DefaultTenantHeader, "not-a-configured-tenant")

	if got := r.resolve("pane-other", h); got != "" {
		t.Fatalf("resolve = %q, want \"\" — an unconfigured tenant name has no "+
			"caps, so honouring it would be an escape hatch", got)
	}
}

func TestTenant_NilResolverIsUntenanted(t *testing.T) {
	var r *tenantResolver
	if got := r.resolve("p1", http.Header{}); got != "" {
		t.Errorf("nil resolver must resolve to the empty tenant, got %q", got)
	}
	if newTenantResolver(config.ProxyTenantConfig{}) != nil {
		t.Error("an empty tenant config must not build a resolver")
	}
}

func TestTenant_CustomHeaderName(t *testing.T) {
	cfg := tenantCfg(true)
	cfg.Header = "X-Rysh-Customer"
	r := newTenantResolver(cfg)

	h := http.Header{}
	h.Set("X-Rysh-Customer", "globex")
	if got := r.resolve("pane-other", h); got != "globex" {
		t.Errorf("resolve = %q, want globex via the configured header", got)
	}
	h2 := http.Header{}
	h2.Set(DefaultTenantHeader, "globex")
	if got := r.resolve("pane-other", h2); got != "" {
		t.Errorf("the default header must not work once a custom one is set, got %q", got)
	}
}

// TestTenant_ControlHeadersNeverReachTheProvider asserts on the bytes the
// upstream actually received. copyHeaders forwards everything non-hop-by-hop,
// so without an explicit strip the provider would be told which customer a
// request belongs to.
func TestTenant_ControlHeadersNeverReachTheProvider(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	s.SetTenants(tenantCfg(true))

	req := httptest.NewRequest(http.MethodPost, "/anthropic/pane-acme/v1/messages",
		strings.NewReader(`{}`))
	req.Header.Set(DefaultTenantHeader, "acme")
	req.Header.Set("X-Rysh-Something-Else", "internal")
	req.Header.Set("X-Custom-Passthrough", "keep me")
	s.ServeHTTP(httptest.NewRecorder(), req)

	for _, h := range []string{DefaultTenantHeader, "X-Rysh-Something-Else"} {
		if v := got.Get(h); v != "" {
			t.Errorf("%s reached the provider with value %q — rysh control headers "+
				"must never cross the wire", h, v)
		}
	}
	if v := got.Get("X-Custom-Passthrough"); v != "keep me" {
		t.Errorf("a caller's own header was dropped: %q", v)
	}
}

// TestTenant_CeilingRefusesWhilePaneHasHeadroom is the accounting property: a
// pane under its own ceiling must still be refused when its CUSTOMER is out of
// budget, or a tenant cap is escaped by opening another pane.
func TestTenant_CeilingRefusesWhilePaneHasHeadroom(t *testing.T) {
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
	}))
	defer upstream.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	s.SetTenants(tenantCfg(true))
	// The ledger says: the PANE is fine, the TENANT is not.
	s.budget, _ = newTestChecker(&msg.MsgUsageCheckReply{
		PaneID: "pane-acme", SpentTokens: 60, CeilingTokens: 50, Ok: false,
		Scope: msg.UsageScopeTenant, Tenant: "acme",
	}, nil)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/anthropic/pane-acme/v1/messages", strings.NewReader(`{}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if upstreamHits != 0 {
		t.Fatalf("an over-tenant-budget request reached the provider (%d hits)", upstreamHits)
	}
	// The message must name the TENANT's budget, not the pane's — otherwise it
	// points the operator at the wrong command and the wrong number.
	body := rec.Body.String()
	if !strings.Contains(body, "tenant acme") {
		t.Errorf("refusal names the wrong budget: %s", body)
	}
	if strings.Contains(body, "##cost budget") {
		t.Errorf("a tenant refusal must not suggest the per-pane command: %s", body)
	}

	audits := s.RecentAudits(1)
	if len(audits) != 1 || audits[0].Tenant != "acme" {
		t.Errorf("the refusal must be attributed to the tenant in the audit: %+v", audits)
	}
}

// TestTenant_RateLimitBindsAcrossPanesOfOneTenant: a customer with several
// panes must not get several times the throughput.
func TestTenant_RateLimitBindsAcrossPanesOfOneTenant(t *testing.T) {
	cfg := config.ProxyTenantConfig{
		Tenants: map[string]config.TenantRule{
			"acme": {
				Panes:     []string{"p1", "p2"},
				RateLimit: config.RateLimitRule{Rate: 1, Burst: 1},
			},
		},
	}
	s := New(nil, nil, nil, nil, false)
	s.SetTenants(cfg)

	if ok, _, _ := s.rate.allow("p1", "anthropic", "acme"); !ok {
		t.Fatal("first request must be admitted")
	}
	ok, _, scope := s.rate.allow("p2", "anthropic", "acme")
	if ok {
		t.Fatal("a second pane of the same tenant must share the tenant's budget")
	}
	if scope != scopeTenant {
		t.Errorf("scope = %q, want %q", scope, scopeTenant)
	}
	// A different tenant is unaffected.
	if ok, _, _ := s.rate.allow("p9", "anthropic", ""); !ok {
		t.Error("an untenanted pane must not be throttled by another tenant")
	}
}

// TestTenant_UsageRecordCarriesTenant closes the accounting loop: the record
// the ledger indexes must name the customer.
func TestTenant_UsageRecordCarriesTenant(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","usage":{"input_tokens":5,"output_tokens":2}}`))
	}))
	defer upstream.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	s.SetTenants(tenantCfg(true))

	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost,
		"/anthropic/pane-acme/v1/messages", strings.NewReader(`{}`)))

	audits := s.RecentAudits(1)
	if len(audits) != 1 {
		t.Fatal("no audit recorded")
	}
	if audits[0].Tenant != "acme" {
		t.Errorf("audit Tenant = %q, want acme", audits[0].Tenant)
	}
}

// TestBudgetCache_IsKeyedByTenant: two tenants sharing a pane id must not share
// a cached verdict, or one customer's refusal would block the other.
func TestBudgetCache_IsKeyedByTenant(t *testing.T) {
	b, calls := newTestChecker(&msg.MsgUsageCheckReply{PaneID: "p1", Ok: true}, nil)

	b.check("p1", "acme")
	b.check("p1", "globex")
	if *calls != 2 {
		t.Fatalf("ledger calls = %d, want 2 — the cache must be keyed by tenant", *calls)
	}
	b.check("p1", "acme")
	if *calls != 2 {
		t.Fatalf("ledger calls = %d, want 2 — the same tenant must still be cached", *calls)
	}

	// Invalidating the pane must clear its tenanted entries too, or a pane that
	// just spent keeps a stale verdict for the full TTL.
	b.invalidate("p1")
	b.check("p1", "acme")
	if *calls != 3 {
		t.Fatalf("ledger calls = %d, want 3 — invalidate must drop tenanted keys", *calls)
	}
}
