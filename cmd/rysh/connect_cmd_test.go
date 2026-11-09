// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	cfgPath, _, err := writeCompanyWorkspace(dir, claim)
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
	if _, _, err := writeCompanyWorkspace(dir, claim); err != nil {
		t.Fatalf("second write of the same company: %v", err)
	}
	// Another company's claim into this directory is refused.
	other := *claim
	other.Company.ID = "33333333-3333-3333-3333-333333333333"
	other.Company.Slug = "other"
	if _, _, err := writeCompanyWorkspace(dir, &other); err == nil || !strings.Contains(err.Error(), "already belongs to company") {
		t.Fatalf("a foreign directory must be refused, got %v", err)
	}
}

// CD2 (sprint 0.2.15): the connect key goes to .rysh/secrets; the YAML holds
// only the ${RYSH_UPSTREAM_API_KEY} reference, and an older workspace with a
// plain key is migrated on the next connect.
func TestWriteCompanyWorkspaceKeepsTheKeyOutOfTheYAML(t *testing.T) {
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("RYSH_DIR", "")
	dir := filepath.Join(t.TempDir(), "firat")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// What an older `rysh connect` left: the key in plain YAML.
	old := "upstream:\n  enabled: true\n  url: https://letchat.ai\n  api_key: rysh_old_plain\n  workspace: 11111111-1111-1111-1111-111111111111\n" +
		"company:\n  id: 22222222-2222-2222-2222-222222222222\n  slug: firat\n"
	if err := os.WriteFile(filepath.Join(dir, "rysh.config.yaml"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := fakeClaimServer(t)
	defer srv.Close()
	claim, err := claimCompany(srv.URL, "rysh_ct_good", "terminal", "x")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath, secretsPath, err := writeCompanyWorkspace(dir, claim)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw), "rysh_key") || strings.Contains(string(raw), "rysh_old_plain") {
		t.Fatal("rysh.config.yaml still carries a key value")
	}
	if !strings.Contains(string(raw), "api_key: ${RYSH_UPSTREAM_API_KEY}") {
		t.Fatalf("rysh.config.yaml has no ${RYSH_UPSTREAM_API_KEY} reference:\n%s", raw)
	}
	if want := filepath.Join(dir, ".rysh", "secrets", "default", "RYSH_UPSTREAM_API_KEY"); secretsPath != want {
		t.Fatalf("secrets path = %q, want %q", secretsPath, want)
	}
	for p, mode := range map[string]os.FileMode{
		secretsPath:                                       0o600,
		filepath.Join(dir, ".rysh", "secrets"):            0o700,
		filepath.Join(dir, ".rysh", "secrets", "default"): 0o700,
	} {
		info, err := os.Lstat(p)
		if err != nil || !(info.Mode().IsRegular() || info.IsDir()) || info.Mode().Perm() != mode {
			t.Fatalf("%s: %v mode %v, want %o", p, err, info.Mode(), mode)
		}
	}
	if b, _ := os.ReadFile(secretsPath); string(b) != "rysh_key" {
		t.Fatal("the secrets file does not hold the claimed key")
	}
	if got := config.LoadFrom(cfgPath).Upstream.APIKey; got != "rysh_key" {
		t.Fatalf("the reference does not resolve on load: %q", got)
	}
}

// ledger 157 (security-lead) + desktop: `connect --json` never prints the key
// and names the exact secrets file; the claim sends the server ONLY the code,
// the vehicle and the device label, even from a directory that already holds
// a key, secrets and a config.
func TestConnectSendsOnlyTheCodeAndDeviceAndPrintsNoKey(t *testing.T) {
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("RYSH_DIR", "")
	dir := filepath.Join(t.TempDir(), "firat")
	if err := os.MkdirAll(filepath.Join(dir, ".rysh", "secrets", "default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rysh.config.yaml"),
		[]byte("upstream:\n  api_key: rysh_existing_plain\ncompany:\n  id: 22222222-2222-2222-2222-222222222222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".rysh", "secrets", "default", "OTHER_SECRET"), []byte("hush_other"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	type seen struct {
		method, path string
		header       http.Header
		body         []byte
	}
	var reqs []seen
	inner := fakeClaimServer(t)
	defer inner.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, seen{r.Method, r.URL.Path, r.Header.Clone(), b})
		r2, _ := http.NewRequest(r.Method, inner.URL+r.URL.Path, bytes.NewReader(b))
		r2.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(r2)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runConnect(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
			[]string{srv.URL, "rysh_ct_good", "--json", "--device", "test-box", "--dir", dir}); err != nil {
			t.Fatalf("runConnect: %v", err)
		}
	})

	if len(reqs) != 1 || reqs[0].method != http.MethodPost || reqs[0].path != "/api/company-connect/claim" {
		t.Fatalf("connect made %d request(s): %+v", len(reqs), reqs)
	}
	var body map[string]any
	if err := json.Unmarshal(reqs[0].body, &body); err != nil {
		t.Fatalf("claim body is not JSON: %v", err)
	}
	if len(body) != 3 || body["token"] != "rysh_ct_good" || body["vehicle"] != "terminal" || body["device"] != "test-box" {
		t.Fatalf("claim body must be exactly token, vehicle, device: %v", body)
	}
	for _, h := range []string{"Authorization", "Cookie", "X-Rysh-Token", "X-Api-Key"} {
		if reqs[0].header.Get(h) != "" {
			t.Fatalf("claim carried a %s header", h)
		}
	}
	all := string(reqs[0].body) + fmt.Sprint(reqs[0].header)
	for _, leak := range []string{"rysh_existing_plain", "hush_other", "22222222-2222-2222-2222-222222222222"} {
		if strings.Contains(all, leak) {
			t.Fatalf("the claim request carried local state %q", leak)
		}
	}

	if strings.Contains(out, "rysh_key") || strings.Contains(out, "api_key") {
		t.Fatalf("--json output carries the key:\n%s", out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("--json output: %v\n%s", err, out)
	}
	for _, k := range []string{"directory", "config_file", "session", "server_url", "workspace_id", "company", "secrets_file"} {
		if _, ok := res[k]; !ok {
			t.Fatalf("--json lacks %q: %s", k, out)
		}
	}
	if want := filepath.Join(dir, ".rysh", "secrets", "default", "RYSH_UPSTREAM_API_KEY"); res["secrets_file"] != want {
		t.Fatalf("secrets_file = %v, want %s", res["secrets_file"], want)
	}
}

// ledger 167 X1/X3: a crafted config that points upstream at another server
// and names some other secret as its key sends nothing anywhere: not through
// `rysh company … --config` (what ##config load registers as the MCP server).
func TestCraftedConfigCannotSendAnotherSecretUpstream(t *testing.T) {
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("RYSH_DIR", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-env-value-for-x3")
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path+" "+r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if _, err := config.WriteSecret(filepath.Join(dir, ".rysh"), "other", "STORED_TOKEN", "fake-scope-value-for-x3"); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"${AWS_SECRET_ACCESS_KEY}", "${STORED_TOKEN}", "${RYSH_UPSTREAM_API_KEY}${STORED_TOKEN}"} {
		p := filepath.Join(dir, "evil.rysh.config.yaml")
		body := "upstream:\n  enabled: true\n  url: " + srv.URL + "\n  api_key: \"" + ref + "\"\n  workspace: 11111111-1111-1111-1111-111111111111\n" +
			"company:\n  id: 22222222-2222-2222-2222-222222222222\n  slug: firat\n  server: " + srv.URL + "\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runCompanyCLI(config.Config{}, []string{"status", "--json", "--config", p}); err == nil {
			t.Errorf("api_key %s: rysh company status must refuse", ref)
		}
	}
	if len(hits) != 0 {
		t.Fatalf("the crafted configs reached the server %d time(s)", len(hits))
	}
}

// R3: with RYSH_UPSTREAM_API_KEY in the environment, a config that names its
// own server never receives that key (the file's key, or none, is used).
func TestEnvUpstreamKeyNeverGoesToAFileChosenServer(t *testing.T) {
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_DIR", "")
	t.Setenv("RYSH_UPSTREAM_URL", "")
	t.Setenv("RYSH_UPSTREAM_API_KEY", "fake-env-upstream-r3")
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "evil.rysh.config.yaml")
	body := "upstream:\n  enabled: true\n  url: " + srv.URL + "\n  api_key: rysh_file_key\n  workspace: 11111111-1111-1111-1111-111111111111\n" +
		"company:\n  id: 22222222-2222-2222-2222-222222222222\n  slug: firat\n  server: " + srv.URL + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = runCompanyCLI(config.Config{}, []string{"status", "--json", "--config", p})
	if len(auths) == 0 {
		t.Fatal("no request reached the server: this proves nothing")
	}
	for _, a := range auths {
		if strings.Contains(a, "fake-env-upstream-r3") {
			t.Fatal("the environment's upstream key went to a server the config file chose")
		}
	}
}

// desktop's CD3 rehearsal: a link opened over https must never be downgraded
// or redirected by the claim. The server_url the claim names is accepted only
// when it is https on the same host (and port) as the link; an http link (a
// local or dev server) keeps today's behaviour.
func TestCheckClaimServerNeverDowngrades(t *testing.T) {
	for _, tc := range []struct {
		link, claimed string
		ok            bool
		why           string
	}{
		{"https://rysh.ai", "https://rysh.ai", true, ""},
		{"https://rysh.ai/", "https://RYSH.ai/", true, ""},
		{"https://dev.rysh.ai", "http://dev.rysh.ai", false, "https"},
		{"https://rysh.ai", "https://evil.example", false, "host"},
		{"https://rysh.ai", "https://rysh.ai:8443", false, "host"},
		{"https://rysh.ai", "ftp://rysh.ai", false, "https"},
		{"https://rysh.ai", "not a url", false, ""},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", true, ""},
		{"http://localhost:8080", "http://localhost:8080", true, ""},
		// security-lead ledger 190: http only to loopback, link or claim.
		{"http://dev.rysh.ai", "http://dev.rysh.ai", false, "loopback"},
		{"http://127.0.0.1:8080", "http://dev.rysh.ai", false, "loopback"},
	} {
		err := checkClaimServer(tc.link, tc.claimed)
		if tc.ok != (err == nil) {
			t.Errorf("link %s, claim %s: err=%v, want ok=%v", tc.link, tc.claimed, err, tc.ok)
			continue
		}
		if err != nil && tc.why != "" && !strings.Contains(err.Error(), tc.why) {
			t.Errorf("link %s, claim %s: the refusal must say why (%q): %v", tc.link, tc.claimed, tc.why, err)
		}
	}
}

// ledger 190: over a real https link, a claim naming http on the same host is
// refused after the claim, and nothing more is sent: no workspace is written,
// no request carries the key.
func TestConnectRefusesAnHTTPClaimFromAnHTTPSLink(t *testing.T) {
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("RYSH_DIR", "")
	var reqs []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		host := r.Host
		_, _ = w.Write([]byte(`{"server_url":"http://` + host + `","api_key":"rysh_key","workspace_id":"11111111-1111-1111-1111-111111111111",
			"company":{"id":"22222222-2222-2222-2222-222222222222","name":"Modern Bistro","slug":"firat","fqdn":"firat.dns.rysh.ai","status":"active"}}`))
	}))
	defer srv.Close()
	old := connectHTTPClient
	connectHTTPClient = func() *http.Client { c := srv.Client(); c.Timeout = 15 * time.Second; return c }
	t.Cleanup(func() { connectHTTPClient = old })
	dir := filepath.Join(t.TempDir(), "firat")
	err := runConnect(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]string{srv.URL, "rysh_ct_good", "--json", "--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "https") || strings.Contains(err.Error(), "rysh_key") {
		t.Fatalf("an http server_url from an https link must be refused, naming why and never the key: %v", err)
	}
	if len(reqs) != 1 || !strings.HasPrefix(reqs[0], "POST /api/company-connect/claim ") {
		t.Fatalf("after the refused claim nothing more may be sent: %v", reqs)
	}
	if _, err := os.Stat(filepath.Join(dir, "rysh.config.yaml")); !os.IsNotExist(err) {
		t.Fatal("a refused claim still wrote a workspace")
	}
}
