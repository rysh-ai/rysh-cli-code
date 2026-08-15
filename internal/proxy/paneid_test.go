// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// countingUpstream is a stub provider that records how many requests reached it.
// Every assertion below is about bytes that did or did not leave the machine,
// not about which branch the handler took.
func countingUpstream(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func postThrough(t *testing.T, base, path string) *http.Response {
	t.Helper()
	resp, err := http.Post(base+path, "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestPaneID_EmptySegmentIsRefused is the defect this validation exists for.
//
// `POST /anthropic//v1/messages` from inside a governed pane used to reach the
// provider having skipped BOTH controls: budget.go returns allowed for an empty
// pane, and ratelimit.go skips the per-pane bucket for the same value. One
// deleted path component opted a pane out of every per-pane control it had.
func TestPaneID_EmptySegmentIsRefused(t *testing.T) {
	up, hits := countingUpstream(t)

	srv := New(nil, nil, nil, map[string]string{"anthropic": up.URL}, false)
	// A rate rule that would refuse the SECOND request of any pane, so the test
	// cannot pass merely because nothing was configured.
	srv.SetRateLimit(config.ProxyRateLimitConfig{
		PerPane: config.RateLimitRule{Rate: 0.001, Burst: 1},
	})
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()
	srv.NotePane("pane-1")

	resp := postThrough(t, srv.BaseURL(), "/anthropic//v1/messages")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if *hits != 0 {
		t.Fatalf("an unattributable request reached the upstream %dx", *hits)
	}

	// Non-vacuous: the same request WITH a registered pane is forwarded.
	if r := postThrough(t, srv.BaseURL(), "/anthropic/pane-1/v1/messages"); r.StatusCode != 200 {
		t.Fatalf("registered pane status = %d, want 200", r.StatusCode)
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", *hits)
	}

	// And the per-pane bucket really does bind for that pane — which is the
	// control the empty segment was skipping.
	if r := postThrough(t, srv.BaseURL(), "/anthropic/pane-1/v1/messages"); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429 (the rule the empty pane escaped)", r.StatusCode)
	}
}

// TestPaneID_ForeignPaneCannotInheritAnotherTenantsPin: the pin is what "a pin
// always beats the header" (022 §4.3) rests on, and it is keyed by pane ID. If
// any pane may name any pane, the pin is selectable by the thing it caps.
func TestPaneID_ForeignPaneCannotInheritAnotherTenantsPin(t *testing.T) {
	up, hits := countingUpstream(t)

	srv := New(nil, nil, nil, map[string]string{"anthropic": up.URL}, false)
	srv.SetTenants(config.ProxyTenantConfig{
		Tenants: map[string]config.TenantRule{
			"acme": {Panes: []string{"pane-acme"}, CeilingTokens: 1_000_000},
		},
	})
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()
	srv.NotePane("pane-mine")

	resp := postThrough(t, srv.BaseURL(), "/anthropic/pane-acme/v1/messages")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a pane the daemon never injected", resp.StatusCode)
	}
	if *hits != 0 {
		t.Fatalf("borrowed-identity request reached the upstream %dx", *hits)
	}
	// The attempt is visible after the fact, which is the point of auditing it.
	audits := srv.RecentAudits(10)
	if len(audits) != 1 || audits[0].Endpoint != "(unknown-pane)" {
		t.Fatalf("refusal not audited: %+v", audits)
	}
}

// TestPaneID_EmptyRegistryAllowsSyntacticallyValidPanes keeps the probe proxy
// (`##proxy check`), cmd/wire-harness and every direct-construction test
// working: none of them run a pane lifecycle, so enforcing against an empty
// registry would refuse all traffic.
func TestPaneID_EmptyRegistryAllowsSyntacticallyValidPanes(t *testing.T) {
	up, hits := countingUpstream(t)
	srv := New(nil, nil, nil, map[string]string{"anthropic": up.URL}, false)
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	if r := postThrough(t, srv.BaseURL(), "/anthropic/proxy-check/v1/messages"); r.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 with no panes registered", r.StatusCode)
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", *hits)
	}
	// Even with an empty registry, an unusable ID is still refused.
	if r := postThrough(t, srv.BaseURL(), "/anthropic//v1/messages"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("empty pane status = %d, want 403 regardless of registry state", r.StatusCode)
	}
}

func TestValidPaneID(t *testing.T) {
	ok := []string{"pane-1", "p", "tab.1_lane-2", "A0"}
	bad := []string{"", ".", "..", "a/b", "a b", "pane*", "pane#1",
		strings.Repeat("p", maxPaneIDLen+1)}
	for _, s := range ok {
		if !validPaneID(s) {
			t.Errorf("validPaneID(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if validPaneID(s) {
			t.Errorf("validPaneID(%q) = true, want false", s)
		}
	}
}

// TestPaneID_RefusalIsProviderShaped: the caller is a wrapped CLI. A refusal it
// cannot parse turns a clean stop into a crash.
func TestPaneID_RefusalIsProviderShaped(t *testing.T) {
	up, _ := countingUpstream(t)
	srv := New(nil, nil, nil, map[string]string{
		"anthropic": up.URL, "openai": up.URL, "gemini": up.URL,
	}, false)
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()
	srv.NotePane("pane-1")

	for _, c := range []struct {
		dialect string
		check   func(t *testing.T, m map[string]interface{})
	}{
		{"anthropic", func(t *testing.T, m map[string]interface{}) {
			if m["type"] != "error" ||
				m["error"].(map[string]interface{})["type"] != "permission_error" {
				t.Fatalf("not anthropic-shaped: %v", m)
			}
		}},
		{"openai", func(t *testing.T, m map[string]interface{}) {
			if m["error"].(map[string]interface{})["code"] != "permission_denied" {
				t.Fatalf("not openai-shaped: %v", m)
			}
		}},
		{"gemini", func(t *testing.T, m map[string]interface{}) {
			e := m["error"].(map[string]interface{})
			if e["status"] != "PERMISSION_DENIED" || e["code"].(float64) != 403 {
				t.Fatalf("not gemini-shaped: %v", m)
			}
		}},
	} {
		t.Run(c.dialect, func(t *testing.T) {
			resp := postThrough(t, srv.BaseURL(), "/"+c.dialect+"/ghost/v1/x")
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content-type = %q, want application/json", ct)
			}
			var m map[string]interface{}
			b, _ := io.ReadAll(resp.Body)
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("body is not JSON (%v): %s", err, b)
			}
			c.check(t, m)
		})
	}
}

// TestForgetPane_ReleasesEverything covers Tier 0.4: ForgetPane had no callers,
// and even when called it left govWatch.seen behind — so a recycled pane ID
// reported itself governed on the strength of the previous occupant's traffic.
func TestForgetPane_ReleasesEverything(t *testing.T) {
	up, _ := countingUpstream(t)
	srv := New(nil, nil, nil, map[string]string{"anthropic": up.URL}, false)
	srv.SetRateLimit(config.ProxyRateLimitConfig{
		PerPane: config.RateLimitRule{Rate: 1, Burst: 1},
	})
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()
	srv.NotePane("pane-1")

	if r := postThrough(t, srv.BaseURL(), "/anthropic/pane-1/v1/messages"); r.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", r.StatusCode)
	}
	if st := srv.PaneGovernanceState("pane-1"); st != "governed" {
		t.Fatalf("state = %q, want governed", st)
	}
	if _, ok := srv.rate.panes["pane-1"]; !ok {
		t.Fatal("expected a per-pane bucket to exist before ForgetPane")
	}

	srv.ForgetPane("pane-1")

	if _, ok := srv.rate.panes["pane-1"]; ok {
		t.Error("rate bucket survived ForgetPane")
	}
	if st := srv.PaneGovernanceState("pane-1"); st != "idle" {
		t.Errorf("state = %q after ForgetPane, want idle — a recycled pane ID "+
			"must not inherit the previous occupant's governed verdict", st)
	}
	// The registration is released too, so a closed pane's ID stops being a
	// usable attribution target.
	if r := postThrough(t, srv.BaseURL(), "/anthropic/pane-1/v1/messages"); r.StatusCode != http.StatusForbidden {
		t.Errorf("closed pane still accepted: status = %d, want 403", r.StatusCode)
	}
}

// TestForgetPane_DropsTheCachedLedgerVerdict pins the third thing ForgetPane
// releases, which no HTTP-level assertion can see.
func TestForgetPane_DropsTheCachedLedgerVerdict(t *testing.T) {
	srv := New(nil, nil, nil, nil, false)
	calls := 0
	srv.budget.request = func(string, interface{}, time.Duration) (interface{}, error) {
		calls++
		return &msg.MsgUsageCheckReply{PaneID: "pane-1", Ok: true}, nil
	}

	if allowed, _ := srv.budget.check("pane-1", ""); !allowed {
		t.Fatal("first check should allow")
	}
	if _, _ = srv.budget.check("pane-1", ""); calls != 1 {
		t.Fatalf("ledger calls = %d, want 1 (the 2s cache)", calls)
	}
	srv.ForgetPane("pane-1")
	if _, _ = srv.budget.check("pane-1", ""); calls != 2 {
		t.Fatalf("ledger calls = %d after ForgetPane, want 2 — the cached "+
			"verdict outlived the pane", calls)
	}
}
