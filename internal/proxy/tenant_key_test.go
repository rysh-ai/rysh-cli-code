// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// Per-tenant upstream credentials (design 022 §8.3).
//
// Until now a tenant selected a CAP on a shared key. Selecting the KEY is what
// makes a tenant a boundary rather than an accounting label: a reseller bills
// each customer on that customer's own provider account, and one customer's
// request is never signed with another's credential.

// authCapture records the credential each upstream request carried.
type authCapture struct {
	mu   sync.Mutex
	keys []string
}

func (a *authCapture) add(k string) {
	a.mu.Lock()
	a.keys = append(a.keys, k)
	a.mu.Unlock()
}

func (a *authCapture) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.keys...)
}

func tenantKeyProxy(t *testing.T, cap *authCapture) (*Server, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.add(r.Header.Get("x-api-key"))
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)

	srv := New(nil, nil, map[string]string{"anthropic": "SHARED-KEY"},
		map[string]string{"anthropic": upstream.URL}, false)
	srv.SetTenants(config.ProxyTenantConfig{
		Tenants: map[string]config.TenantRule{
			"acme": {
				Panes: []string{"pane-acme"},
				Keys:  map[string]string{"anthropic": "ACME-KEY"},
			},
			// A tenant with a cap but no key of its own still uses the shared one.
			"globex": {Panes: []string{"pane-globex"}, CeilingTokens: 1000},
		},
	})
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv, upstream
}

func TestTenantKey_PinnedPaneUsesItsTenantsCredential(t *testing.T) {
	cap := &authCapture{}
	srv, _ := tenantKeyProxy(t, cap)

	for _, pane := range []string{"pane-acme", "pane-globex", "pane-none"} {
		resp, err := http.Post(srv.BaseURL()+"/anthropic/"+pane+"/v1/messages",
			"application/json", strings.NewReader(`{"model":"m"}`))
		if err != nil {
			t.Fatalf("post %s: %v", pane, err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}

	got := cap.all()
	want := []string{"ACME-KEY", "SHARED-KEY", "SHARED-KEY"}
	if len(got) != len(want) {
		t.Fatalf("upstream saw %d requests, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d carried key %q, want %q", i, got[i], want[i])
		}
	}
}

// TestTenantKey_HeaderCannotSelectAnotherTenantsKey is the security half. A
// tenant name that could be chosen by the caller would now choose a CREDENTIAL,
// which makes the pin rule (022 §4.3) load-bearing for key selection too.
func TestTenantKey_HeaderCannotSelectAnotherTenantsKey(t *testing.T) {
	cap := &authCapture{}
	srv, _ := tenantKeyProxy(t, cap)

	req, _ := http.NewRequest(http.MethodPost,
		srv.BaseURL()+"/anthropic/pane-globex/v1/messages",
		strings.NewReader(`{"model":"m"}`))
	req.Header.Set(DefaultTenantHeader, "acme")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	got := cap.all()
	if len(got) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(got))
	}
	if got[0] == "ACME-KEY" {
		t.Fatal("a header selected another tenant's upstream credential — the " +
			"pin must win, or a pane can bill any customer it names")
	}
	if got[0] != "SHARED-KEY" {
		t.Fatalf("key = %q, want SHARED-KEY", got[0])
	}
}

// TestTenantKey_FailoverTargetKeyStillWins: a fallback upstream that carries its
// own credential authenticates differently from the vendor endpoint. Signing
// that attempt with the tenant's vendor key would fail exactly the request
// failover exists to rescue.
func TestTenantKey_FailoverTargetKeyStillWins(t *testing.T) {
	cap := &authCapture{}

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.add(r.Header.Get("x-api-key"))
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.add(r.Header.Get("x-api-key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer fallback.Close()

	srv := New(nil, nil, map[string]string{"anthropic": "SHARED-KEY"},
		map[string]string{"anthropic": primary.URL}, false)
	srv.SetTenants(config.ProxyTenantConfig{
		Tenants: map[string]config.TenantRule{
			"acme": {Panes: []string{"pane-acme"}, Keys: map[string]string{"anthropic": "ACME-KEY"}},
		},
	})
	srv.SetFailover(config.ProxyFailoverConfig{
		Enabled: true,
		Upstreams: map[string][]config.FailoverUpstream{
			"anthropic": {{URL: fallback.URL, Key: "CORP-GATEWAY-KEY"}},
		},
	})
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	resp, err := http.Post(srv.BaseURL()+"/anthropic/pane-acme/v1/messages",
		"application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover", resp.StatusCode)
	}

	got := cap.all()
	if len(got) != 2 {
		t.Fatalf("attempts = %d, want 2", len(got))
	}
	if got[0] != "ACME-KEY" {
		t.Errorf("primary attempt key = %q, want the tenant's ACME-KEY", got[0])
	}
	if got[1] != "CORP-GATEWAY-KEY" {
		t.Errorf("fallback attempt key = %q, want the upstream's own key", got[1])
	}
}

func TestHasTenantKeys(t *testing.T) {
	if HasTenantKeys(config.ProxyTenantConfig{}) {
		t.Error("no tenants ⇒ no tenant keys")
	}
	capsOnly := config.ProxyTenantConfig{
		Tenants: map[string]config.TenantRule{"a": {CeilingTokens: 1}},
	}
	if HasTenantKeys(capsOnly) {
		t.Error("a cap is not a credential")
	}
	withKey := config.ProxyTenantConfig{
		Tenants: map[string]config.TenantRule{"a": {Keys: map[string]string{"openai": "k"}}},
	}
	if !HasTenantKeys(withKey) {
		t.Error("a configured tenant key must be visible in ##proxy status")
	}
}
