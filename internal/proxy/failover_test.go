package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// recordingUpstream captures every request body it receives, so a test can
// assert on the bytes that actually left — not on what the handler intended.
type recordingUpstream struct {
	mu     sync.Mutex
	bodies []string
	hits   int32
	handle func(w http.ResponseWriter, r *http.Request, hit int)
}

func newRecordingUpstream(h func(w http.ResponseWriter, r *http.Request, hit int)) (*recordingUpstream, *httptest.Server) {
	u := &recordingUpstream{handle: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := int(atomic.AddInt32(&u.hits, 1))
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, string(b))
		u.mu.Unlock()
		u.handle(w, r, hit)
	}))
	return u, srv
}

func (u *recordingUpstream) count() int { return int(atomic.LoadInt32(&u.hits)) }

func (u *recordingUpstream) body(i int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if i >= len(u.bodies) {
		return ""
	}
	return u.bodies[i]
}

// TestFailover_DisabledIsExactlyOneAttempt is the no-regression guard: with no
// failover config the proxy must behave byte-for-byte as it did before 022 —
// one attempt, and a 503 handed straight back to the caller.
func TestFailover_DisabledIsExactlyOneAttempt(t *testing.T) {
	up, srv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer srv.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": srv.URL}, false)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(`{"model":"m"}`)))

	if up.count() != 1 {
		t.Fatalf("upstream attempts = %d, want exactly 1 with failover off", up.count())
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the upstream's own 503 passed through", rec.Code)
	}
}

// TestFailover_RetriesTo Secondary covers the headline case: primary 503, the
// secondary serves, and the client sees one clean 200.
func TestFailover_RetriesToSecondary(t *testing.T) {
	primary, psrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	})
	defer psrv.Close()

	secondary, ssrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`{"model":"claude-x","usage":{"input_tokens":7,"output_tokens":3}}`))
	})
	defer ssrv.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": psrv.URL}, false)
	s.SetFailover(config.ProxyFailoverConfig{
		Enabled:   true,
		Upstreams: map[string][]config.FailoverUpstream{"anthropic": {{URL: ssrv.URL}}},
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(`{"model":"claude-x"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the secondary", rec.Code)
	}
	if primary.count() != 1 || secondary.count() != 1 {
		t.Fatalf("attempts primary=%d secondary=%d, want 1 each", primary.count(), secondary.count())
	}
	if !strings.Contains(rec.Body.String(), "claude-x") {
		t.Fatalf("client got the wrong body: %s", rec.Body.String())
	}
	// The client must see ONE response, not the 503 spliced onto the 200.
	if strings.Contains(rec.Body.String(), "down") {
		t.Fatalf("the failed upstream's body leaked to the client: %s", rec.Body.String())
	}

	audits := s.RecentAudits(1)
	if len(audits) != 1 {
		t.Fatal("no audit recorded")
	}
	if audits[0].Attempts != 2 {
		t.Errorf("audit Attempts = %d, want 2", audits[0].Attempts)
	}
	if audits[0].Upstream != ssrv.URL {
		t.Errorf("audit Upstream = %q, want the secondary %q", audits[0].Upstream, ssrv.URL)
	}
	if !strings.Contains(audits[0].String(), "after 2 attempts") {
		t.Errorf("audit line should surface the failover: %s", audits[0].String())
	}
}

// TestFailover_ReplayedBodyIsStillRedacted is the security-critical case. A
// retry re-sends the request body; if it replayed the PRE-redaction bytes the
// secondary upstream would receive the very secret SNAT exists to withhold.
func TestFailover_ReplayedBodyIsStillRedacted(t *testing.T) {
	// Same live-shaped key the design 001 wire test plants, so the detector
	// tier is known to fire on it and this test cannot pass vacuously.
	const planted = "sk_live_ABCD1234abcd5678EFGH9012"

	primary, psrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadGateway)
	})
	defer psrv.Close()
	secondary, ssrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	defer ssrv.Close()

	mgr, err := secretnat.NewManager(secretnat.Options{Enabled: true, Mode: secretnat.ModeSemantic})
	if err != nil {
		t.Fatalf("secretnat: %v", err)
	}
	s := New(nil, mgr, nil, map[string]string{"anthropic": psrv.URL}, false)
	s.SetFailover(config.ProxyFailoverConfig{
		Enabled:   true,
		Upstreams: map[string][]config.FailoverUpstream{"anthropic": {{URL: ssrv.URL}}},
	})

	body := `{"model":"m","messages":[{"role":"user","content":"my key is ` + planted + `"}]}`
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(body)))

	for name, got := range map[string]string{
		"primary":   primary.body(0),
		"secondary": secondary.body(0),
	} {
		if got == "" {
			t.Fatalf("%s received no body", name)
		}
		if strings.Contains(got, planted) {
			t.Fatalf("%s received the PLANTED SECRET — a replayed body escaped redaction: %s", name, got)
		}
		// Positive evidence: redaction actually ran on this attempt, rather
		// than the secret merely never being present.
		if !strings.Contains(got, "sk_live_SNAT") {
			t.Fatalf("%s body carries no SNAT token — redaction did not run: %s", name, got)
		}
	}
	// Both attempts must carry the same redacted bytes, not two different ones.
	if primary.body(0) != secondary.body(0) {
		t.Errorf("replayed body differs from the original attempt:\n primary=%s\n secondary=%s",
			primary.body(0), secondary.body(0))
	}
}

// TestFailover_NoRetryOnceStreamingStarted is THE invariant (design 022 §4.1).
// An upstream that returns 200 and then dies mid-body has already had bytes
// forwarded to the client. Retrying there would splice two completions into one
// response — worse than the truncation it tries to fix.
func TestFailover_NoRetryOnceStreamingStarted(t *testing.T) {
	primary, psrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"partial\":true}\n\n"))
		w.(http.Flusher).Flush()
		// Die mid-stream: hijack and slam the connection so the client sees a
		// truncated body rather than a clean EOF.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	})
	defer psrv.Close()

	secondary, ssrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`{"should":"never be reached"}`))
	})
	defer ssrv.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": psrv.URL}, false)
	s.SetFailover(config.ProxyFailoverConfig{
		Enabled:   true,
		Upstreams: map[string][]config.FailoverUpstream{"anthropic": {{URL: ssrv.URL}}},
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(`{"model":"m","stream":true}`)))

	if primary.count() != 1 {
		t.Fatalf("primary attempts = %d, want 1", primary.count())
	}
	if secondary.count() != 0 {
		t.Fatalf("a mid-stream death triggered a retry (%d secondary hits) — "+
			"the client would have received two spliced completions", secondary.count())
	}
	if !strings.Contains(rec.Body.String(), "partial") {
		t.Errorf("the partial stream must still reach the client: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "never be reached") {
		t.Error("the secondary's body was spliced onto a partially-sent response")
	}
}

// TestFailover_LastAttemptStatusPassesThrough: when every upstream fails, the
// caller must see the provider's own error. Synthesizing a 502 would replace an
// error the wrapped CLI knows how to read with one it does not.
func TestFailover_LastAttemptStatusPassesThrough(t *testing.T) {
	_, psrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer psrv.Close()
	_, ssrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	})
	defer ssrv.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": psrv.URL}, false)
	s.SetFailover(config.ProxyFailoverConfig{
		Enabled:   true,
		Upstreams: map[string][]config.FailoverUpstream{"anthropic": {{URL: ssrv.URL}}},
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(`{}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the last upstream's own 429 verbatim", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Errorf("the provider's own error body must survive: %s", rec.Body.String())
	}
}

// TestFailover_MaxAttemptsBounds the loop even with a long chain configured.
func TestFailover_MaxAttemptsBoundsTheChain(t *testing.T) {
	var hits [3]*recordingUpstream
	var urls []config.FailoverUpstream
	primary, psrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadGateway)
	})
	defer psrv.Close()
	hits[0] = primary
	for i := 1; i < 3; i++ {
		u, srv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.WriteHeader(http.StatusBadGateway)
		})
		defer srv.Close()
		hits[i] = u
		urls = append(urls, config.FailoverUpstream{URL: srv.URL})
	}

	s := New(nil, nil, nil, map[string]string{"anthropic": psrv.URL}, false)
	s.SetFailover(config.ProxyFailoverConfig{
		Enabled:     true,
		MaxAttempts: 2,
		Upstreams:   map[string][]config.FailoverUpstream{"anthropic": urls},
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(`{}`)))

	if hits[0].count() != 1 || hits[1].count() != 1 {
		t.Fatalf("first two upstreams should each be tried once, got %d and %d",
			hits[0].count(), hits[1].count())
	}
	if hits[2].count() != 0 {
		t.Fatalf("max_attempts=2 must stop before the third upstream (got %d hits)", hits[2].count())
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want the last attempt's 502", rec.Code)
	}
}

// TestFailover_ClientDisconnectDoesNotRetry: a caller that has gone away wants
// nothing, least of all a second provider call it will never read — and that
// call would still be billed.
func TestFailover_ClientDisconnectDoesNotRetry(t *testing.T) {
	primary, psrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer psrv.Close()
	secondary, ssrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`{}`))
	})
	defer ssrv.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": psrv.URL}, false)
	s.SetFailover(config.ProxyFailoverConfig{
		Enabled:   true,
		Upstreams: map[string][]config.FailoverUpstream{"anthropic": {{URL: ssrv.URL}}},
	})

	// A context already cancelled stands in for the client having hung up
	// between the first attempt and the retry decision.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(`{}`)).WithContext(ctx)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if primary.count() != 0 || secondary.count() != 0 {
		t.Fatalf("a cancelled request must reach no upstream, got primary=%d secondary=%d",
			primary.count(), secondary.count())
	}
}

// TestFailover_PerUpstreamKey: a fallback with its own credential must be sent
// THAT credential. Sending it the vendor key would fail exactly the request
// failover exists to rescue.
func TestFailover_PerUpstreamKey(t *testing.T) {
	_, psrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer psrv.Close()

	var gotKey string
	_, ssrv := newRecordingUpstream(func(w http.ResponseWriter, r *http.Request, _ int) {
		gotKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{}`))
	})
	defer ssrv.Close()

	s := New(nil, nil, map[string]string{"anthropic": "vendor-key"},
		map[string]string{"anthropic": psrv.URL}, false)
	s.SetFailover(config.ProxyFailoverConfig{
		Enabled: true,
		Upstreams: map[string][]config.FailoverUpstream{
			"anthropic": {{URL: ssrv.URL, Key: "gateway-key"}},
		},
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(`{}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotKey != "gateway-key" {
		t.Errorf("fallback received key %q, want its own %q", gotKey, "gateway-key")
	}
}

// TestFailover_InheritsDialectKeyWhenUnset keeps the common case a one-liner:
// a fallback with no key of its own uses the dialect credential.
func TestFailover_InheritsDialectKeyWhenUnset(t *testing.T) {
	_, psrv := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer psrv.Close()

	var gotKey string
	_, ssrv := newRecordingUpstream(func(w http.ResponseWriter, r *http.Request, _ int) {
		gotKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{}`))
	})
	defer ssrv.Close()

	s := New(nil, nil, map[string]string{"anthropic": "vendor-key"},
		map[string]string{"anthropic": psrv.URL}, false)
	s.SetFailover(config.ProxyFailoverConfig{
		Enabled:   true,
		Upstreams: map[string][]config.FailoverUpstream{"anthropic": {{URL: ssrv.URL}}},
	})

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(`{}`)))

	if gotKey != "vendor-key" {
		t.Errorf("fallback received key %q, want the inherited %q", gotKey, "vendor-key")
	}
}

// TestShouldRetry pins the classifier. 501 stays out: "not implemented" is a
// stable answer, and retrying it just multiplies the same error.
func TestShouldRetry(t *testing.T) {
	s := New(nil, nil, nil, nil, false)

	retryable := []int{429, 500, 502, 503, 504}
	for _, c := range retryable {
		if !s.shouldRetry(c) {
			t.Errorf("status %d should be retryable by default", c)
		}
	}
	for _, c := range []int{200, 201, 400, 401, 403, 404, 422, 501} {
		if s.shouldRetry(c) {
			t.Errorf("status %d must NOT be retried", c)
		}
	}

	s.SetFailover(config.ProxyFailoverConfig{Enabled: true, RetryStatus: []int{503}})
	if !s.shouldRetry(503) || s.shouldRetry(500) {
		t.Error("an explicit retry_status list must replace the defaults, not extend them")
	}
}

// TestFailover_HeaderTimeoutIsOffByDefault guards the reasoning in §4.1: any
// default here would eventually reclassify a slow non-streaming completion as a
// dead upstream and retry it, paying for the same generation twice.
func TestFailover_HeaderTimeoutIsOffByDefault(t *testing.T) {
	s := New(nil, nil, nil, nil, false)
	if s.tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("ResponseHeaderTimeout = %v, want 0 (opt-in only)", s.tr.ResponseHeaderTimeout)
	}
	s.SetFailover(config.ProxyFailoverConfig{Enabled: true})
	if s.tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("enabling failover alone must not install a header timeout, got %v",
			s.tr.ResponseHeaderTimeout)
	}
	s.SetFailover(config.ProxyFailoverConfig{Enabled: true, HeaderTimeout: 5 * time.Second})
	if s.tr.ResponseHeaderTimeout != 5*time.Second {
		t.Fatalf("explicit header_timeout not applied, got %v", s.tr.ResponseHeaderTimeout)
	}
}

// TestTargetsFor_NeverQueuesThePrimaryTwice: a fallback list that repeats the
// primary would otherwise retry the upstream that just failed.
func TestTargetsFor_NeverQueuesThePrimaryTwice(t *testing.T) {
	s := New(nil, nil, nil, nil, false)
	s.SetFailover(config.ProxyFailoverConfig{
		Enabled: true,
		Upstreams: map[string][]config.FailoverUpstream{
			"anthropic": {{URL: "https://api.anthropic.com"}, {URL: "https://gw.example"}},
		},
	})
	got := s.targetsFor("anthropic", "https://api.anthropic.com")
	if len(got) != 2 {
		t.Fatalf("targets = %+v, want the primary plus one distinct fallback", got)
	}
	if got[0].URL != "https://api.anthropic.com" || got[1].URL != "https://gw.example" {
		t.Fatalf("unexpected target order: %+v", got)
	}
}
