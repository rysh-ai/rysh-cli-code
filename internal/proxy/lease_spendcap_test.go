// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"
)

// root-order-044 #8: the workspace USD spend cap at the proxy boundary.

// capGate is a fakeGate that also reports a spend cap, as the real
// LedgerClient does.
type capGate struct {
	fakeGate
	cap    sharedmsg.GatewaySpendCap
	active bool
}

func (g *capGate) SpendCap(string) (sharedmsg.GatewaySpendCap, bool) { return g.cap, g.active }

func postModel(t *testing.T, base, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(base+"/anthropic/pane-1/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func errorMessage(t *testing.T, resp *http.Response) (string, string) {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	e, _ := body["error"].(map[string]any)
	typ, _ := e["type"].(string)
	m, _ := e["message"].(string)
	return typ, m
}

// The refusal keeps the rate-limit shape a wrapped CLI already handles, and
// names the cap, the estimated spend and the period.
func TestSpendCap_RefusalNamesTheCapInDollars(t *testing.T) {
	gate := &capGate{
		fakeGate: fakeGate{allow: false, reason: sharedmsg.GatewayRefusalSpendCap},
		cap:      sharedmsg.GatewaySpendCap{Period: "month", CapMicroUSD: 10_000_000, SpentMicroUSD: 12_500_000},
		active:   true,
	}
	srv, hits := leaseProxy(t, gate)
	resp := postModel(t, srv.BaseURL(), `{"model":"claude-sonnet-4-5","messages":[]}`)
	if resp.StatusCode != http.StatusTooManyRequests || *hits != 0 {
		t.Fatalf("status %d, upstream hits %d; want 429 and 0", resp.StatusCode, *hits)
	}
	typ, m := errorMessage(t, resp)
	if typ != "rate_limit_error" {
		t.Fatalf("type = %q, want the rate-limit shape", typ)
	}
	for _, want := range []string{"spend cap", "$12.50 of $10.00", "this month (UTC)", "estimated"} {
		if !strings.Contains(m, want) {
			t.Errorf("message %q lacks %q", m, want)
		}
	}
	if strings.Contains(m, "token ceiling") {
		t.Errorf("a spend-cap refusal must not blame the token ceiling: %q", m)
	}
	if a := srv.RecentAudits(5); len(a) != 1 || a[0].Endpoint != "(lease)" {
		t.Fatalf("not audited as a lease refusal: %+v", a)
	}
}

func TestSpendCap_DayPeriodWording(t *testing.T) {
	m := spendCapMessage("", sharedmsg.GatewaySpendCap{Period: "day", CapMicroUSD: 1_000_000, SpentMicroUSD: 1_000_000}, true)
	if !strings.Contains(m, "today (UTC)") || !strings.Contains(m, "$1.00 of $1.00") {
		t.Fatalf("day message = %q", m)
	}
	// No figures known: still names the cap, invents no numbers.
	m = spendCapMessage("acme", sharedmsg.GatewaySpendCap{}, false)
	if !strings.Contains(m, "spend cap") || strings.Contains(m, "$") {
		t.Fatalf("no-figures message = %q", m)
	}
}

// Under an active cap, a model with no price would count as $0 and slip past
// the cap, so it is refused before the provider is called.
func TestSpendCap_UnpricedModelRefusedUnderCap(t *testing.T) {
	gate := &capGate{fakeGate: fakeGate{allow: true}, cap: sharedmsg.GatewaySpendCap{Period: "month", CapMicroUSD: 1}, active: true}
	srv, hits := leaseProxy(t, gate)
	resp := postModel(t, srv.BaseURL(), `{"model":"no-such-model-x","messages":[]}`)
	if resp.StatusCode != http.StatusForbidden || *hits != 0 {
		t.Fatalf("status %d, upstream hits %d; want 403 and 0", resp.StatusCode, *hits)
	}
	_, m := errorMessage(t, resp)
	for _, want := range []string{"no-such-model-x", "no price", "spend cap", "pricing"} {
		if !strings.Contains(m, want) {
			t.Errorf("message %q lacks %q", m, want)
		}
	}
	if a := srv.RecentAudits(5); len(a) != 1 || a[0].Endpoint != "(model)" || a[0].Model != "no-such-model-x" {
		t.Fatalf("not audited as a model refusal: %+v", a)
	}
}

func TestSpendCap_PricedModelPassesUnderCap(t *testing.T) {
	gate := &capGate{fakeGate: fakeGate{allow: true}, cap: sharedmsg.GatewaySpendCap{Period: "month", CapMicroUSD: 1}, active: true}
	srv, hits := leaseProxy(t, gate)
	if r := postModel(t, srv.BaseURL(), `{"model":"claude-sonnet-4-5","messages":[]}`); r.StatusCode != 200 || *hits != 1 {
		t.Fatalf("status %d, hits %d; want 200 and 1", r.StatusCode, *hits)
	}
}

// Without a cap, an unpriced model passes exactly as before.
func TestSpendCap_UnpricedModelPassesWithoutCap(t *testing.T) {
	for name, gate := range map[string]LeaseGate{
		"gate reports no cap":   &capGate{fakeGate: fakeGate{allow: true}, active: false},
		"gate without SpendCap": &fakeGate{allow: true},
	} {
		srv, hits := leaseProxy(t, gate)
		if r := postModel(t, srv.BaseURL(), `{"model":"no-such-model-x","messages":[]}`); r.StatusCode != 200 || *hits != 1 {
			t.Errorf("%s: status %d, hits %d; want 200 and 1", name, r.StatusCode, *hits)
		}
	}
}

// A request that names no model is not checked by this rule (stated limit).
func TestSpendCap_NoModelInBodyIsNotChecked(t *testing.T) {
	gate := &capGate{fakeGate: fakeGate{allow: true}, cap: sharedmsg.GatewaySpendCap{Period: "month", CapMicroUSD: 1}, active: true}
	srv, hits := leaseProxy(t, gate)
	if r := postModel(t, srv.BaseURL(), `{"messages":[]}`); r.StatusCode != 200 || *hits != 1 {
		t.Fatalf("status %d, hits %d; want 200 and 1", r.StatusCode, *hits)
	}
}
