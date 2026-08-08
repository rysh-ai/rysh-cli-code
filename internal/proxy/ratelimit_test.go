package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// fakeClock drives the limiter without sleeping. x/time/rate takes an explicit
// `now` on every call, so the injected clock is exact rather than approximate.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestLimiter(cfg config.ProxyRateLimitConfig) (*rateLimiter, *fakeClock) {
	l := newRateLimiter(cfg, nil)
	clk := newFakeClock()
	if l != nil {
		l.now = clk.now
	}
	return l, clk
}

// TestRateLimit_UnsetIsUnlimited is the default-posture guard: a control that
// did not exist yesterday must not throttle a session that used to work.
func TestRateLimit_UnsetIsUnlimited(t *testing.T) {
	l, _ := newTestLimiter(config.ProxyRateLimitConfig{})
	if l != nil {
		t.Fatal("an empty config must not build a limiter at all")
	}
	for i := 0; i < 1000; i++ {
		if ok, _, _ := l.allow("p1", "anthropic", ""); !ok {
			t.Fatalf("nil limiter refused request %d", i)
		}
	}
}

// TestRateLimit_BurstThenThrottle exercises the core bucket contract on a fake
// clock: the burst is admitted immediately, the next request is refused, and
// the bucket refills at the configured rate.
func TestRateLimit_BurstThenThrottle(t *testing.T) {
	l, clk := newTestLimiter(config.ProxyRateLimitConfig{
		PerPane: config.RateLimitRule{Rate: 2, Burst: 3},
	})

	for i := 0; i < 3; i++ {
		if ok, _, _ := l.allow("p1", "anthropic", ""); !ok {
			t.Fatalf("burst request %d must be admitted", i+1)
		}
	}

	ok, wait, scope := l.allow("p1", "anthropic", "")
	if ok {
		t.Fatal("the request after the burst must be refused")
	}
	if scope != scopePane {
		t.Errorf("scope = %q, want %q", scope, scopePane)
	}
	// Rate 2/s ⇒ the next token is ~500ms out.
	if wait <= 0 || wait > 600*time.Millisecond {
		t.Errorf("retryAfter = %v, want ~500ms", wait)
	}

	clk.advance(500 * time.Millisecond)
	if ok, _, _ := l.allow("p1", "anthropic", ""); !ok {
		t.Error("a refilled bucket must admit again")
	}
}

// TestRateLimit_PanesAreIsolated: one runaway pane must not throttle another.
// Per-pane governance is the whole point of the paneID in the route.
func TestRateLimit_PanesAreIsolated(t *testing.T) {
	l, _ := newTestLimiter(config.ProxyRateLimitConfig{
		PerPane: config.RateLimitRule{Rate: 1, Burst: 2},
	})

	for i := 0; i < 2; i++ {
		if ok, _, _ := l.allow("busy", "anthropic", ""); !ok {
			t.Fatalf("burst request %d must be admitted", i+1)
		}
	}
	if ok, _, _ := l.allow("busy", "anthropic", ""); ok {
		t.Fatal("the busy pane should now be throttled")
	}
	if ok, _, _ := l.allow("quiet", "anthropic", ""); !ok {
		t.Fatal("a saturated pane must not throttle a different pane")
	}
}

// TestRateLimit_GlobalBindsAcrossPanes: the session cap is the backstop that
// per-pane rules cannot provide, since N panes each under their own limit can
// still overwhelm a provider together.
func TestRateLimit_GlobalBindsAcrossPanes(t *testing.T) {
	l, _ := newTestLimiter(config.ProxyRateLimitConfig{
		Global: config.RateLimitRule{Rate: 1, Burst: 2},
	})

	if ok, _, _ := l.allow("p1", "anthropic", ""); !ok {
		t.Fatal("first request must be admitted")
	}
	if ok, _, _ := l.allow("p2", "openai", ""); !ok {
		t.Fatal("second request must be admitted (burst 2)")
	}
	ok, _, scope := l.allow("p3", "gemini", "")
	if ok {
		t.Fatal("the session cap must bind across panes and dialects")
	}
	if scope != scopeSession {
		t.Errorf("scope = %q, want %q", scope, scopeSession)
	}
}

// TestRateLimit_PerDialect binds one provider without touching the others.
func TestRateLimit_PerDialect(t *testing.T) {
	l, _ := newTestLimiter(config.ProxyRateLimitConfig{
		PerDialect: map[string]config.RateLimitRule{
			"anthropic": {Rate: 1, Burst: 1},
		},
	})

	if ok, _, _ := l.allow("p1", "anthropic", ""); !ok {
		t.Fatal("first anthropic request must be admitted")
	}
	ok, _, scope := l.allow("p1", "anthropic", "")
	if ok || scope != scopeDialect {
		t.Fatalf("second anthropic request: ok=%v scope=%q, want refused by dialect", ok, scope)
	}
	if ok, _, _ := l.allow("p1", "openai", ""); !ok {
		t.Fatal("an unconfigured dialect must stay unlimited")
	}
}

// TestRateLimit_RefusalDoesNotConsumeEarlierScopes is the reservation-cancel
// invariant. Scopes are reserved in order, so when a LATER scope refuses, the
// tokens already taken from earlier ones must go back — otherwise a pane is
// charged for traffic that never left, and it ends up throttled for requests
// the session cap already refused.
func TestRateLimit_RefusalDoesNotConsumeEarlierScopes(t *testing.T) {
	l, clk := newTestLimiter(config.ProxyRateLimitConfig{
		// Effectively no refill: the pane bucket is a pool of exactly 5.
		PerPane: config.RateLimitRule{Rate: 0.0001, Burst: 5},
		// Refills once a second, so it is the scope that keeps refusing.
		Global: config.RateLimitRule{Rate: 1, Burst: 1},
	})

	admitted := 0
	for i := 0; i < 12; i++ {
		if ok, _, _ := l.allow("p1", "anthropic", ""); ok {
			admitted++
		}
		clk.advance(time.Second) // let the session bucket refill each round
	}

	// The pane pool holds 5. Every round the session cap refuses costs nothing,
	// so all 5 pane tokens must eventually be spendable.
	if admitted != 5 {
		t.Fatalf("admitted %d requests, want 5 — a refused request consumed a "+
			"pane token it should have returned", admitted)
	}
}

// TestRateLimit_ServeHTTPRefusesBeforeUpstream is the end-to-end guard: a
// throttled request is dialect-shaped, carries an honest Retry-After, and never
// reaches the provider.
func TestRateLimit_ServeHTTPRefusesBeforeUpstream(t *testing.T) {
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	s.SetRateLimit(config.ProxyRateLimitConfig{
		PerPane: config.RateLimitRule{Rate: 1, Burst: 1},
	})
	clk := newFakeClock()
	s.rate.now = clk.now

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
			strings.NewReader(`{"model":"claude-x"}`))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	if upstreamHits != 1 {
		t.Fatalf("upstream hits = %d, want 1", upstreamHits)
	}

	rec := do()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled request status = %d, want 429", rec.Code)
	}
	if upstreamHits != 1 {
		t.Fatalf("a throttled request reached the provider (%d hits)", upstreamHits)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1 (from the bucket, not a canned hour)", got)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("429 body is not valid JSON: %v", err)
	}
	if m["type"] != "error" ||
		m["error"].(map[string]interface{})["type"] != "rate_limit_error" {
		t.Fatalf("throttle 429 is not anthropic-shaped: %v", m)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit") {
		t.Errorf("body should read as a rate limit: %s", rec.Body.String())
	}

	// The refusal must be visible in rysh, not only to the wrapped CLI.
	audits := s.RecentAudits(10)
	if len(audits) == 0 || audits[len(audits)-1].Endpoint != "(ratelimit)" {
		t.Fatalf("throttle not recorded in the audit ring: %+v", audits)
	}
}

// TestEffectiveBurst pins the "rate without burst" default. A bucket with
// burst 0 admits nothing, which is never what `rate: 5` alone meant.
func TestEffectiveBurst(t *testing.T) {
	cases := []struct {
		rule config.RateLimitRule
		want int
	}{
		{config.RateLimitRule{Rate: 5}, 5},
		{config.RateLimitRule{Rate: 0.5}, 1},
		{config.RateLimitRule{Rate: 2.4}, 3},
		{config.RateLimitRule{Rate: 5, Burst: 20}, 20},
	}
	for _, c := range cases {
		if got := c.rule.EffectiveBurst(); got != c.want {
			t.Errorf("%+v EffectiveBurst = %d, want %d", c.rule, got, c.want)
		}
	}
}
