// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// The terminal half of Company Connect (design 037): claim a code, write the
// company workspace, refuse someone else's directory.
func fakeClaimServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/company-connect/claim" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req struct{ Token, Vehicle, Device string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Vehicle != "terminal" || req.Device == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"vehicle must be \"desktop\" or \"terminal\""}`))
			return
		}
		if req.Token != "rysh_ct_good" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"connect code is invalid, expired or already used"}`))
			return
		}
		_, _ = w.Write([]byte(`{"server_url":"https://letchat.ai","api_key":"rysh_key","workspace_id":"11111111-1111-1111-1111-111111111111",
			"company":{"id":"22222222-2222-2222-2222-222222222222","workspace_id":"11111111-1111-1111-1111-111111111111","name":"Modern Bistro","slug":"firat","fqdn":"firat.dns.rysh.ai","status":"active","template_id":"restaurant"},
			"subjects":{"activity":"ws.1.ai-native-company.2.activity","progress":"ws.1.ai-native-company.2.*.progress"}}`))
	}))
}

func TestClaimCompanyRelaysTheServersVerdict(t *testing.T) {
	srv := fakeClaimServer(t)
	defer srv.Close()
	claim, err := claimCompany(srv.URL+"/", "rysh_ct_good", "terminal", "test-box (terminal)")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim.APIKey != "rysh_key" || claim.Company.Slug != "firat" || claim.ServerURL != "https://letchat.ai" {
		t.Fatalf("claim: %+v", claim)
	}
	if _, err := claimCompany(srv.URL, "rysh_ct_used", "terminal", "x"); err == nil || !strings.Contains(err.Error(), "invalid, expired or already used") {
		t.Fatalf("a refused code must surface the server's one message, got %v", err)
	}
	if _, err := claimCompany("letchat.ai", "rysh_ct_good", "terminal", "x"); err == nil || !strings.Contains(err.Error(), "must be a URL") {
		t.Fatalf("a bare host must be refused before any request, got %v", err)
	}
}

func TestWriteCompanyWorkspaceWritesAndRefusesForeignDirectories(t *testing.T) {
	t.Setenv(connectDirEnv, t.TempDir())
	// The loader lets RYSH_SESSION override session_name; a test run from
	// inside a rysh pane must not see the pane's session here.
	t.Setenv("RYSH_SESSION", "")
	dir, err := companyWorkspaceDir("", "firat")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != "firat" || !strings.HasPrefix(dir, os.Getenv(connectDirEnv)) {
		t.Fatalf("dir: %s", dir)
	}
	if _, err := companyWorkspaceDir("", "../etc"); err == nil {
		t.Fatal("a slug with a path in it must be refused")
	}

	srv := fakeClaimServer(t)
	defer srv.Close()
	claim, err := claimCompany(srv.URL, "rysh_ct_good", "terminal", "x")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath, err := writeCompanyWorkspace(dir, claim)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(cfgPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config must exist and be owner-only: %v %v", err, info.Mode())
	}
	cfg := config.LoadFrom(cfgPath)
	if cfg.Company.ID != claim.Company.ID || cfg.Company.Slug != "firat" || cfg.Upstream.APIKey != "rysh_key" || cfg.Upstream.Workspace != claim.WorkspaceID || cfg.SessionName != "firat" || !cfg.Upstream.Enabled {
		t.Fatalf("written config: company=%+v upstream=%+v session=%q", cfg.Company, cfg.Upstream, cfg.SessionName)
	}

	// Re-connecting the same company rewrites in place.
	if _, err := writeCompanyWorkspace(dir, claim); err != nil {
		t.Fatalf("second write of the same company: %v", err)
	}
	// Another company's claim into this directory is refused.
	other := *claim
	other.Company.ID = "33333333-3333-3333-3333-333333333333"
	other.Company.Slug = "other"
	if _, err := writeCompanyWorkspace(dir, &other); err == nil || !strings.Contains(err.Error(), "already belongs to company") {
		t.Fatalf("a foreign directory must be refused, got %v", err)
	}
}
