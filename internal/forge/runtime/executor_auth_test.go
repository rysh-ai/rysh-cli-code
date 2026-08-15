// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

var protectedOp = &ir.Operation{ID: "getProtected", Method: "GET", Path: "/protected"}

// TestExecutor_TokenProviderInjectsBearer verifies the executor fetches a token
// from the provider and injects it as the bearer (Model A / owner identity).
func TestExecutor_TokenProviderInjectsBearer(t *testing.T) {
	s := newAuthServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	t.Setenv("TM_USER", "a")
	t.Setenv("TM_PASS", "b")

	tm, err := NewTokenManager(AuthConfig{
		Type:        AuthOAuth2Password,
		TokenURL:    srv.URL + "/oauth/token",
		UsernameEnv: "TM_USER",
		PasswordEnv: "TM_PASS",
	}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ex := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{
		TokenProvider:  tm.Token,
		TokenRefresher: tm.Refresh,
	})
	out, err := ex.Call(context.Background(), protectedOp, map[string]any{}, "")
	if err != nil {
		t.Fatalf("Call() error: %v", err)
	}
	if !strings.Contains(out, `"ok": true`) {
		t.Fatalf("unexpected protected response: %s", out)
	}
}

// TestExecutor_ReactiveRefreshOn401 verifies that a 401 triggers a single token
// refresh + retry, after which the request succeeds.
func TestExecutor_ReactiveRefreshOn401(t *testing.T) {
	s := newAuthServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	t.Setenv("TM_USER", "a")
	t.Setenv("TM_PASS", "b")

	tm, err := NewTokenManager(AuthConfig{
		Type:        AuthOAuth2Password,
		TokenURL:    srv.URL + "/oauth/token",
		UsernameEnv: "TM_USER",
		PasswordEnv: "TM_PASS",
	}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	// Prime the cache, then invalidate the access token server-side (but keep the
	// refresh token valid) so the first protected call 401s.
	if _, err := tm.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.valid = map[string]bool{} // access token rejected
	s.mu.Unlock()

	ex := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{
		TokenProvider:  tm.Token,
		TokenRefresher: tm.Refresh,
	})
	out, err := ex.Call(context.Background(), protectedOp, map[string]any{}, "")
	if err != nil {
		t.Fatalf("Call() should have recovered via refresh, got: %v", err)
	}
	if !strings.Contains(out, `"ok": true`) {
		t.Fatalf("unexpected protected response after refresh: %s", out)
	}
	if got := atomic.LoadInt32(&s.refreshes); got != 1 {
		t.Fatalf("refreshes = %d, want exactly 1 (single reactive retry)", got)
	}
}

// TestExecutor_PerCallBearerOverride verifies that a context bearer override
// (Model B) is used verbatim and that the owner does NOT refresh it on 401 (the
// subscriber owns that token).
func TestExecutor_PerCallBearerOverride(t *testing.T) {
	s := newAuthServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	// A token the server accepts, supplied out-of-band (as a subscriber would).
	s.mu.Lock()
	s.valid["delegated-tok"] = true
	s.mu.Unlock()

	// The provider would return a DIFFERENT token; the override must win.
	providerCalls := int32(0)
	ex := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{
		TokenProvider: func(context.Context) (string, error) {
			atomic.AddInt32(&providerCalls, 1)
			return "owner-tok", nil
		},
		TokenRefresher: func(context.Context) (string, error) {
			t.Fatal("owner must NOT refresh a delegated (per-call) token")
			return "", nil
		},
	})
	ctx := WithBearer(context.Background(), "delegated-tok")
	out, err := ex.Call(ctx, protectedOp, map[string]any{}, "")
	if err != nil {
		t.Fatalf("Call() with override error: %v", err)
	}
	if !strings.Contains(out, `"bearer": "delegated-tok"`) {
		t.Fatalf("server did not see the delegated bearer: %s", out)
	}
	if atomic.LoadInt32(&providerCalls) != 0 {
		t.Fatalf("provider should not be consulted when an override is present")
	}
}
