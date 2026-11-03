// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UkOGRLrDDDRvVLHKPtC21iXSUBVl"

func testRun() Run {
	return Run{ID: "423b54f6fd36fa94", WorkspaceID: "ws1", Name: "n", State: "running", Host: "dev.rysh.ai", Port: 32770, HostKey: testHostKey}
}

func TestStartSendsKeyHeaderAndBodyOnly(t *testing.T) {
	var gotPath, gotKey, gotAuth, gotIdem string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotAuth, gotIdem = r.URL.Path, r.Header.Get("X-API-Key"), r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(testRun())
	}))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: "k-secret", WorkspaceID: "ws1"}}
	run, err := c.Start(context.Background(), StartOptions{Name: "n", LLMProvider: "anthropic", LLMModel: "m", LLMKey: "sk-llm"}, "ssh-ed25519 AAAA pub")
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "423b54f6fd36fa94" || run.Port != 32770 {
		t.Fatalf("run = %+v", run)
	}
	if gotPath != "/api/workspaces/ws1/hosted-runs" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer k-secret" || gotKey != "" {
		t.Errorf("the workspace key must travel as Authorization: Bearer only (key=%q auth=%q)", gotKey, gotAuth)
	}
	if gotIdem == "" {
		t.Error("a start must carry an Idempotency-Key so a retry cannot start two runs")
	}
	if body["ssh_public_key"] != "ssh-ed25519 AAAA pub" || body["llm_provider"] != "anthropic" || body["llm_api_key"] != "sk-llm" {
		t.Errorf("body = %v", body)
	}
}

func TestStartWithoutLLMSendsNoLLMFields(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(testRun())
	}))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: "k", WorkspaceID: "ws1"}}
	if _, err := c.Start(context.Background(), StartOptions{Name: "n"}, "pub"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"llm_provider", "llm_model", "llm_api_key"} {
		if _, ok := body[k]; ok {
			t.Errorf("shell-only run must not send %s", k)
		}
	}
}

func TestErrorsAreFriendlyAndNeverCarryTheKey(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{401, "refused"},
		{403, "read-only"},
		{404, "not enabled"},
		{429, "quota"},
		{502, "HTTP 502"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"error":"echo k-secret sk-llm"}`)
		}))
		c := &Client{Conn: Conn{URL: srv.URL, APIKey: "k-secret", WorkspaceID: "ws1"}}
		_, err := c.Start(context.Background(), StartOptions{LLMProvider: "openai", LLMKey: "sk-llm"}, "pub")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: err = %v, want substring %q", tc.status, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), "k-secret") || strings.Contains(err.Error(), "sk-llm") {
			t.Errorf("status %d: the error leaked a key: %v", tc.status, err)
		}
	}
}

func TestConnRefusesPlainHTTPToANonLoopbackHost(t *testing.T) {
	for _, u := range []string{"http://rysh.ai", "http://dev.rysh.ai:8080", "ftp://x", ""} {
		if err := (Conn{URL: u, APIKey: "k", WorkspaceID: "ws"}).Validate(); err == nil {
			t.Errorf("Validate(%q) = nil; a key must not travel in clear text", u)
		}
	}
	for _, u := range []string{"https://rysh.ai", "http://127.0.0.1:8080", "http://localhost:3000"} {
		if err := (Conn{URL: u, APIKey: "k", WorkspaceID: "ws"}).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v", u, err)
		}
	}
	if err := (Conn{URL: "https://rysh.ai", WorkspaceID: "ws"}).Validate(); err == nil {
		t.Error("an empty api key must be refused")
	}
	if err := (Conn{URL: "https://rysh.ai", APIKey: "k"}).Validate(); err == nil {
		t.Error("an empty workspace id must be refused")
	}
}

func TestListGetStop(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/hosted-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": []Run{testRun()}})
		default:
			_ = json.NewEncoder(w).Encode(testRun())
		}
	}))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: "k", WorkspaceID: "ws1"}}
	runs, err := c.List(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("List = %v, %v", runs, err)
	}
	if _, err := c.Get(context.Background(), "423b54f6fd36fa94"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stop(context.Background(), "423b54f6fd36fa94"); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /api/workspaces/ws1/hosted-runs", "GET /api/workspaces/ws1/hosted-runs/423b54f6fd36fa94", "DELETE /api/workspaces/ws1/hosted-runs/423b54f6fd36fa94"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v", calls)
	}
	if _, err := c.Get(context.Background(), "../etc"); err == nil {
		t.Error("a run id with path characters must be refused before any request")
	}
}

func TestNewKeyPair(t *testing.T) {
	kp, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(kp.Public, "ssh-ed25519 ") || strings.Contains(kp.Public, "\n") {
		t.Fatalf("public = %q", kp.Public)
	}
	if strings.Contains(kp.Public, "PRIVATE") {
		t.Fatal("public line contains private material")
	}
	signer, err := ssh.ParsePrivateKey(kp.Private)
	if err != nil {
		t.Fatalf("private key does not parse: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(kp.Public))
	if err != nil {
		t.Fatal(err)
	}
	if string(signer.PublicKey().Marshal()) != string(pub.Marshal()) {
		t.Error("private and public halves do not match")
	}
	kp2, _ := NewKeyPair()
	if kp2.Public == kp.Public {
		t.Error("two key pairs are identical")
	}
}

func TestStoreSavePinsTheHostKeyAndLocksTheFiles(t *testing.T) {
	dir := t.TempDir()
	st := Store{Dir: dir}
	kp, _ := NewKeyPair()
	p, err := st.Save(testRun(), kp)
	if err != nil {
		t.Fatal(err)
	}
	kh, _ := os.ReadFile(p.KnownHosts)
	if strings.TrimSpace(string(kh)) != "[dev.rysh.ai]:32770 "+testHostKey {
		t.Errorf("known_hosts = %q", kh)
	}
	cfg, _ := os.ReadFile(p.SSHConfig)
	for _, want := range []string{
		"Host hosted-423b54f6fd36fa94", "HostName dev.rysh.ai", "Port 32770", "User rysh",
		"IdentityFile " + p.Key, "IdentitiesOnly yes", "UserKnownHostsFile " + p.KnownHosts, "StrictHostKeyChecking yes",
	} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("ssh_config misses %q:\n%s", want, cfg)
		}
	}
	if !filepath.IsAbs(p.Key) || !filepath.IsAbs(p.KnownHosts) {
		t.Errorf("paths must be absolute: %+v", p)
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{p.Key, p.KnownHosts, p.SSHConfig, p.Meta} {
			fi, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("%s is %v; must be private", f, fi.Mode().Perm())
			}
		}
		di, _ := os.Stat(filepath.Dir(p.Key))
		if di.Mode().Perm()&0o077 != 0 {
			t.Errorf("run dir is %v; must be private", di.Mode().Perm())
		}
	}
	meta, _ := os.ReadFile(p.Meta)
	if strings.Contains(string(meta), "PRIVATE") || strings.Contains(string(meta), "api_key") {
		t.Errorf("run.json must hold no secret: %s", meta)
	}
}

func TestStoreRefusesHostileServerAnswers(t *testing.T) {
	kp, _ := NewKeyPair()
	bad := []Run{}
	r := testRun()
	r.Host = "evil.com\n    ProxyCommand sh -c x"
	bad = append(bad, r)
	r = testRun()
	r.HostKey = testHostKey + "\nssh-rsa AAAA"
	bad = append(bad, r)
	r = testRun()
	r.Port = 0
	bad = append(bad, r)
	r = testRun()
	r.ID = "../../x"
	bad = append(bad, r)
	r = testRun()
	r.HostKey = ""
	bad = append(bad, r)
	for i, run := range bad {
		if _, err := (Store{Dir: t.TempDir()}).Save(run, kp); err == nil {
			t.Errorf("case %d: Save accepted %+v", i, run)
		}
	}
}

func TestStoreLoadListRemove(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	kp, _ := NewKeyPair()
	if _, err := st.Save(testRun(), kp); err != nil {
		t.Fatal(err)
	}
	got, p, err := st.Load("423b54f6fd36fa94")
	if err != nil || got.Port != 32770 || p.Key == "" {
		t.Fatalf("Load = %+v %+v %v", got, p, err)
	}
	all, err := st.List()
	if err != nil || len(all) != 1 {
		t.Fatalf("List = %v %v", all, err)
	}
	if err := st.Remove("423b54f6fd36fa94"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Load("423b54f6fd36fa94"); err == nil {
		t.Error("Load after Remove succeeded")
	}
	if _, err := os.Stat(p.Key); !os.IsNotExist(err) {
		t.Errorf("the private key survived Remove: %v", err)
	}
}

func TestAlias(t *testing.T) {
	if Alias("423b54f6fd36fa94") != "hosted-423b54f6fd36fa94" {
		t.Error("alias")
	}
}
