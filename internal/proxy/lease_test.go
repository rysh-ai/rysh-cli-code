package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The org-wide ceiling at the proxy boundary (design 023 §4.2).
//
// Two properties matter here and nowhere else:
//
//   - the refusal is INDISTINGUISHABLE from the machine-local ceiling's, because
//     a wrapped CLI has to handle exactly one shape of "you are out of budget";
//   - the check is in-process. 023 §7's first risk row is a hot-path regression
//     sneaking in via the lease, and the guard against it is that the gate is a
//     plain interface the proxy calls synchronously — no I/O, no waiting.

// fakeGate is a LeaseGate that records how it was used.
type fakeGate struct {
	mu     sync.Mutex
	allow  bool
	reason string
	calls  int
	spent  int64
	// slow, when set, is how long Allow blocks — used to prove the proxy calls
	// it on the request path (and therefore that a blocking implementation
	// would be a hot-path regression a test can see).
	slow time.Duration
}

func (g *fakeGate) Allow(string) (bool, string) {
	g.mu.Lock()
	g.calls++
	allow, reason, slow := g.allow, g.reason, g.slow
	g.mu.Unlock()
	if slow > 0 {
		time.Sleep(slow)
	}
	return allow, reason
}

func (g *fakeGate) Spend(_ string, tokens int64) {
	g.mu.Lock()
	g.spent += tokens
	g.mu.Unlock()
}

func (g *fakeGate) snapshot() (calls int, spent int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls, g.spent
}

func leaseProxy(t *testing.T, gate LeaseGate) (*Server, *int) {
	t.Helper()
	up, hits := countingUpstream(t)
	srv := New(nil, nil, nil, map[string]string{"anthropic": up.URL}, false)
	srv.SetLeaseGate(gate)
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv, hits
}

// TestLeaseGate_RefusalMatchesTheLocalCeiling: same status, same body shape,
// same Retry-After. The wrapped CLI must not be able to tell an org-wide
// refusal from a per-machine one.
func TestLeaseGate_RefusalMatchesTheLocalCeiling(t *testing.T) {
	gate := &fakeGate{allow: false, reason: "exhausted"}
	srv, hits := leaseProxy(t, gate)

	resp := postThrough(t, srv.BaseURL(), "/anthropic/pane-1/v1/messages")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if *hits != 0 {
		t.Fatalf("a request refused by the org-wide ceiling still reached the provider (%d)", *hits)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Error("no Retry-After on the refusal — a CLI's backoff has nothing to work from")
	}
	var body map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("refusal is not JSON: %s", raw)
	}
	errObj, ok := body["error"].(map[string]interface{})
	if !ok || errObj["type"] != "rate_limit_error" {
		t.Fatalf("not the anthropic rate-limit shape the local ceiling produces: %s", raw)
	}
	if msgText, _ := errObj["message"].(string); !strings.Contains(msgText, "across all machines") {
		t.Errorf("the message should say which ceiling bound: %q", msgText)
	}

	// It is auditable as a refusal, like every other one.
	audits := srv.RecentAudits(5)
	if len(audits) != 1 || audits[0].Endpoint != "(lease)" {
		t.Fatalf("org-wide refusal not audited: %+v", audits)
	}
}

// TestLeaseGate_AllowedRequestSpendsAgainstTheLease: the tokens a request
// actually metered are drawn down locally, which is what makes the next
// refusal happen without a network round trip.
func TestLeaseGate_AllowedRequestSpendsAgainstTheLease(t *testing.T) {
	gate := &fakeGate{allow: true}
	srv, hits := leaseProxy(t, gate)

	if r := postThrough(t, srv.BaseURL(), "/anthropic/pane-1/v1/messages"); r.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", r.StatusCode)
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", *hits)
	}
	// The draw-down happens after the response has been streamed back, so the
	// client can legitimately see the body before the handler finishes.
	// countingUpstream reports 10 in / 5 out.
	deadline := time.Now().Add(2 * time.Second)
	var calls int
	var spent int64
	for time.Now().Before(deadline) {
		calls, spent = gate.snapshot()
		if spent == 15 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if calls != 1 {
		t.Fatalf("Allow called %d times for one request, want 1", calls)
	}
	if spent != 15 {
		t.Fatalf("spent against the lease = %d, want 15", spent)
	}
}

// TestLeaseGate_AbsentByDefault: no gate installed ⇒ the request path is
// byte-for-byte what it was before design 023, which is the OSS guarantee.
func TestLeaseGate_AbsentByDefault(t *testing.T) {
	up, hits := countingUpstream(t)
	srv := New(nil, nil, nil, map[string]string{"anthropic": up.URL}, false)
	if srv.lease != nil {
		t.Fatal("a fresh server must have no lease gate")
	}
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	if r := postThrough(t, srv.BaseURL(), "/anthropic/pane-1/v1/messages"); r.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", r.StatusCode)
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", *hits)
	}
}

// TestLeaseGate_IsOnTheRequestPath is the hot-path canary for 023 §7's first
// risk row. The gate is called synchronously by ServeHTTP, so a gate that
// blocks delays the request — which is exactly why the real implementation must
// never do I/O there. If someone later moves the call off the request path (or
// makes it asynchronous), this fails and the reason is written down here.
func TestLeaseGate_IsOnTheRequestPath(t *testing.T) {
	const delay = 150 * time.Millisecond
	gate := &fakeGate{allow: true, slow: delay}
	srv, _ := leaseProxy(t, gate)

	start := time.Now()
	if r := postThrough(t, srv.BaseURL(), "/anthropic/pane-1/v1/messages"); r.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", r.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("the request finished in %v despite a %v gate — the lease check "+
			"is no longer on the request path, so a slow one would no longer be "+
			"visible as latency (and an in-process check is the whole design)",
			elapsed, delay)
	}
}

// TestLeaseMessage_NamesTheScope: the operator has to know whether the
// workspace or one customer ran out.
func TestLeaseMessage_NamesTheScope(t *testing.T) {
	if got := leaseMessage(""); !strings.Contains(got, "this workspace") {
		t.Errorf("untenanted message = %q", got)
	}
	if got := leaseMessage("acme"); !strings.Contains(got, "tenant acme") {
		t.Errorf("tenanted message = %q", got)
	}
}

// TestPublishRefusalAudit_NilPublisherIsSafe: the refusal paths run with no
// broker in tests and in a degraded daemon.
func TestPublishRefusalAudit_NilPublisherIsSafe(t *testing.T) {
	srv := New(nil, nil, nil, nil, false)
	srv.publishRefusalAudit(AuditLine{PaneID: "p", Dialect: "anthropic"}, msg.ProxyBlocked)
}
