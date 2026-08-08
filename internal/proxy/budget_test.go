package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newTestChecker builds a checker whose ledger call is a stub, and returns a
// pointer to the call counter so tests can assert on caching.
func newTestChecker(reply *msg.MsgUsageCheckReply, err error) (*budgetChecker, *int) {
	calls := 0
	b := &budgetChecker{
		cache: make(map[string]budgetEntry),
		now:   time.Now,
		request: func(string, interface{}, time.Duration) (interface{}, error) {
			calls++
			if err != nil {
				return nil, err
			}
			return reply, nil
		},
	}
	return b, &calls
}

func TestBudgetCheck_BlocksOverCeiling(t *testing.T) {
	b, _ := newTestChecker(&msg.MsgUsageCheckReply{
		PaneID: "p1", SpentTokens: 120_000, CeilingTokens: 100_000, Ok: false,
	}, nil)

	allowed, chk := b.check("p1", "")
	if allowed {
		t.Fatal("a pane over its ceiling must not be allowed to spend")
	}
	if chk.SpentTokens != 120_000 || chk.CeilingTokens != 100_000 {
		t.Fatalf("ledger figures not propagated: %+v", chk)
	}
}

func TestBudgetCheck_AllowsUnderCeilingAndWhenNoCeilingSet(t *testing.T) {
	under, _ := newTestChecker(&msg.MsgUsageCheckReply{
		PaneID: "p1", SpentTokens: 10, CeilingTokens: 100_000, Ok: true,
	}, nil)
	if allowed, _ := under.check("p1", ""); !allowed {
		t.Fatal("a pane under its ceiling must be allowed")
	}

	none, _ := newTestChecker(&msg.MsgUsageCheckReply{
		PaneID: "p2", SpentTokens: 999_999, CeilingTokens: 0, Ok: true,
	}, nil)
	if allowed, _ := none.check("p2", ""); !allowed {
		t.Fatal("no ceiling set (CeilingTokens==0) must always allow")
	}
}

// TestBudgetCheck_FailsOpenWhenLedgerUnreachable pins the documented failure
// mode. Budget enforcement is a cost control, not a security boundary: a ledger
// blip must not take down every agent's provider traffic. If this behaviour is
// ever changed to fail closed, that is a deliberate product decision and this
// test should change with it — it must not drift silently.
func TestBudgetCheck_FailsOpenWhenLedgerUnreachable(t *testing.T) {
	b, _ := newTestChecker(nil, errors.New("nats: timeout"))
	if allowed, _ := b.check("p1", ""); !allowed {
		t.Fatal("an unreachable ledger must allow the request (documented fail-open)")
	}
}

func TestBudgetCheck_NoLedgerConfiguredAllows(t *testing.T) {
	b := newBudgetChecker(nil) // nil publisher ⇒ no request seam
	if allowed, _ := b.check("p1", ""); !allowed {
		t.Fatal("with no ledger wired the proxy must not block traffic")
	}
}

func TestBudgetCheck_CachesPerPaneAndExpires(t *testing.T) {
	reply := &msg.MsgUsageCheckReply{PaneID: "p1", CeilingTokens: 100, SpentTokens: 1, Ok: true}
	b, calls := newTestChecker(reply, nil)

	now := time.Now()
	b.now = func() time.Time { return now }

	b.check("p1", "")
	b.check("p1", "")
	b.check("p1", "")
	if *calls != 1 {
		t.Fatalf("expected the ledger to be queried once within the TTL, got %d", *calls)
	}

	// A different pane is cached separately.
	b.check("p2", "")
	if *calls != 2 {
		t.Fatalf("expected a separate query per pane, got %d", *calls)
	}

	// Past the TTL the entry is refetched.
	now = now.Add(budgetCacheTTL + time.Millisecond)
	b.check("p1", "")
	if *calls != 3 {
		t.Fatalf("expected a refetch after the TTL expired, got %d", *calls)
	}
}

func TestBudgetCheck_InvalidateForcesRefetch(t *testing.T) {
	b, calls := newTestChecker(&msg.MsgUsageCheckReply{PaneID: "p1", Ok: true}, nil)
	b.check("p1", "")
	b.invalidate("p1")
	b.check("p1", "")
	if *calls != 2 {
		t.Fatalf("invalidate must force a refetch, got %d calls", *calls)
	}
}

func TestWriteBudgetExceeded_IsDialectShaped(t *testing.T) {
	tests := []struct {
		dialect string
		assert  func(t *testing.T, m map[string]interface{})
	}{
		{"anthropic", func(t *testing.T, m map[string]interface{}) {
			if m["type"] != "error" {
				t.Fatalf("anthropic body needs top-level type=error, got %v", m)
			}
			e := m["error"].(map[string]interface{})
			if e["type"] != "rate_limit_error" {
				t.Fatalf("anthropic error type wrong: %v", e)
			}
		}},
		{"openai", func(t *testing.T, m map[string]interface{}) {
			e := m["error"].(map[string]interface{})
			if e["code"] != "rate_limit_exceeded" {
				t.Fatalf("openai error code wrong: %v", e)
			}
		}},
		{"gemini", func(t *testing.T, m map[string]interface{}) {
			e := m["error"].(map[string]interface{})
			if e["status"] != "RESOURCE_EXHAUSTED" {
				t.Fatalf("gemini status wrong: %v", e)
			}
			if e["code"].(float64) != 429 {
				t.Fatalf("gemini code wrong: %v", e)
			}
		}},
		// An unregistered-but-routed dialect keeps the default shape.
		{"mystery", func(t *testing.T, m map[string]interface{}) {
			if m["type"] != "error" {
				t.Fatalf("fallback body must keep the default shape, got %v", m)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.dialect, func(t *testing.T) {
			rec := httptest.NewRecorder()
			lookupDialect(tc.dialect).WriteRateLimited(rec, "over budget", budgetRetryAfter)

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", rec.Code)
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Error("429 must carry Retry-After so the CLI backs off")
			}
			var m map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
				t.Fatalf("body is not valid JSON: %v", err)
			}
			tc.assert(t, m)
		})
	}
}

func TestBudgetMessage_NamesTheRecoveryCommand(t *testing.T) {
	got := budgetMessage(msg.MsgUsageCheckReply{SpentTokens: 120_000, CeilingTokens: 100_000})
	for _, want := range []string{"120.0k", "100.0k", "##cost budget"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message must contain %q, got: %s", want, got)
		}
	}
}

// TestServeHTTP_OverBudgetReturns429AndNeverContactsUpstream is the end-to-end
// proof that the ledger verdict actually gates the wire: the upstream server
// must record zero requests.
func TestServeHTTP_OverBudgetReturns429AndNeverContactsUpstream(t *testing.T) {
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	s.budget, _ = newTestChecker(&msg.MsgUsageCheckReply{
		PaneID: "pane-1", SpentTokens: 200_000, CeilingTokens: 100_000, Ok: false,
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/anthropic/pane-1/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if upstreamHits != 0 {
		t.Fatalf("an over-budget request must never reach the provider, got %d hits", upstreamHits)
	}

	body, _ := io.ReadAll(rec.Body)
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("429 body must be valid JSON: %v", err)
	}
	if m["type"] != "error" {
		t.Fatalf("429 body must be anthropic-shaped, got %s", body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 should carry Retry-After so the CLI backs off rather than hammering")
	}

	// The refusal must be visible in rysh's own audit trail, not just to the CLI.
	audits := s.RecentAudits(10)
	if len(audits) != 1 || audits[0].Status != http.StatusTooManyRequests {
		t.Fatalf("budget refusal must be recorded in the audit ring, got %+v", audits)
	}
}

// TestServeHTTP_UnderBudgetStillForwards guards against the gate blocking
// normal traffic.
func TestServeHTTP_UnderBudgetStillForwards(t *testing.T) {
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	s.budget, _ = newTestChecker(&msg.MsgUsageCheckReply{
		PaneID: "pane-1", SpentTokens: 10, CeilingTokens: 100_000, Ok: true,
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/anthropic/pane-1/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("under-budget request should pass through, got %d", rec.Code)
	}
	if upstreamHits != 1 {
		t.Fatalf("expected exactly one upstream call, got %d", upstreamHits)
	}
}
