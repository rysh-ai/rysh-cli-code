// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
)

func secureSpec(baseURL string) []byte {
	return []byte(fmt.Sprintf(`{
  "openapi": "3.0.0",
  "info": {"title": "Secure", "version": "1.0.0"},
  "servers": [{"url": %q}],
  "paths": {
    "/secure": {"get": {"operationId": "getSecure", "responses": {"200": {"description": "ok"}}}}
  }
}`, baseURL))
}

// authIntegrationServer mints a token at /oauth/token and serves /secure only to
// a valid bearer, echoing which bearer it saw.
type authIntegrationServer struct {
	mu    sync.Mutex
	valid map[string]bool
	srv   *httptest.Server
}

func newAuthIntegrationServer() *authIntegrationServer {
	s := &authIntegrationServer{valid: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.valid["minted-access"] = true
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"minted-access","refresh_token":"minted-refresh","expires_in":3600}`))
	})
	mux.HandleFunc("/secure", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		ok := s.valid[tok]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"bearer":%q}`, tok)
	})
	s.srv = httptest.NewServer(mux)
	return s
}

// TestManagerAuth_PhaseA_OwnerToken verifies the full Phase A path: an
// integration declared with an oauth2_password Auth flow enables, and RunOp hits
// the protected endpoint with a freshly-acquired owner bearer.
func TestManagerAuth_PhaseA_OwnerToken(t *testing.T) {
	s := newAuthIntegrationServer()
	defer s.srv.Close()
	t.Setenv("SEC_USER", "alice")
	t.Setenv("SEC_PASS", "pw")

	dir := t.TempDir()
	rel, err := StoreSpec(dir, "secure", secureSpec(s.srv.URL), "json")
	if err != nil {
		t.Fatalf("StoreSpec: %v", err)
	}
	def := Integration{
		Name:     "secure",
		Source:   SourceOpenAPI,
		SpecFile: rel,
		Enabled:  true,
		Auth: &runtime.AuthConfig{
			Type:        runtime.AuthOAuth2Password,
			TokenURL:    s.srv.URL + "/oauth/token",
			UsernameEnv: "SEC_USER",
			PasswordEnv: "SEC_PASS",
		},
	}
	if err := SaveStore(dir, []Integration{def}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}

	mgr := NewManager(sharedtools.NewToolRegistry(), dir, nil)
	if _, _, err := mgr.EnableByName(context.Background(), "secure", mgr.GlobalTarget()); err != nil {
		t.Fatalf("EnableByName: %v", err)
	}

	out, err := mgr.RunOp(context.Background(), "secure_getSecure", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("RunOp: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("RunOp returned tool error: %q", out.Error)
	}
	if !strings.Contains(out.Content, `"bearer": "minted-access"`) {
		t.Fatalf("owner token not injected; got: %s", out.Content)
	}
}

// TestManagerAuth_PhaseB_DelegatedToken verifies RunOpWithAuth injects a
// per-call bearer (Model B / subscriber identity) instead of the owner token.
func TestManagerAuth_PhaseB_DelegatedToken(t *testing.T) {
	s := newAuthIntegrationServer()
	defer s.srv.Close()
	t.Setenv("SEC_USER", "alice")
	t.Setenv("SEC_PASS", "pw")

	// A token valid on the backend, representing the subscriber's identity.
	s.mu.Lock()
	s.valid["sub-token"] = true
	s.mu.Unlock()

	dir := t.TempDir()
	rel, _ := StoreSpec(dir, "secure", secureSpec(s.srv.URL), "json")
	def := Integration{
		Name:     "secure",
		Source:   SourceOpenAPI,
		SpecFile: rel,
		Enabled:  true,
		Auth: &runtime.AuthConfig{
			Type:        runtime.AuthOAuth2Password,
			TokenURL:    s.srv.URL + "/oauth/token",
			UsernameEnv: "SEC_USER",
			PasswordEnv: "SEC_PASS",
		},
	}
	_ = SaveStore(dir, []Integration{def})

	mgr := NewManager(sharedtools.NewToolRegistry(), dir, nil)
	if _, _, err := mgr.EnableByName(context.Background(), "secure", mgr.GlobalTarget()); err != nil {
		t.Fatalf("EnableByName: %v", err)
	}

	out, err := mgr.RunOpWithAuth(context.Background(), "secure_getSecure", json.RawMessage(`{}`), "sub-token")
	if err != nil {
		t.Fatalf("RunOpWithAuth: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("RunOpWithAuth returned tool error: %q", out.Error)
	}
	if !strings.Contains(out.Content, `"bearer": "sub-token"`) {
		t.Fatalf("delegated token not injected; got: %s", out.Content)
	}
}
