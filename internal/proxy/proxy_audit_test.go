// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The audit plane (design 001 §4.5) proves itself only on the wire: the record
// is a data-plane NATS publish, and the whole point is that it survives leaving
// the process. A unit test on AuditLine (the in-process ring) would have stayed
// green the entire time MsgProxyRequestAudit did not exist. These tests assert
// the published record instead.

func inProcNATS(t *testing.T) *nats.Conn {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, DontListen: true,
	})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		ns.Shutdown()
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// subscribeAudit collects every MsgProxyRequestAudit published to any pane.
func subscribeAudit(t *testing.T, nc *nats.Conn) (<-chan msg.MsgProxyRequestAudit, func()) {
	t.Helper()
	ch := make(chan msg.MsgProxyRequestAudit, 8)
	sub, err := nc.Subscribe(msg.ProxyAuditWildcardSubject(), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil || env.TypeTag != msg.TagProxyRequestAudit {
			return
		}
		var rec msg.MsgProxyRequestAudit
		if err := json.Unmarshal(env.Payload, &rec); err != nil {
			return
		}
		ch <- rec
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	_ = nc.Flush()
	return ch, func() { _ = sub.Unsubscribe() }
}

func recvAudit(t *testing.T, ch <-chan msg.MsgProxyRequestAudit) msg.MsgProxyRequestAudit {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a MsgProxyRequestAudit — the proxy never published one")
		return msg.MsgProxyRequestAudit{}
	}
}

// TestProxyPublishesAuditRecord_Success proves the forwarded-request path emits
// a durable audit record with the metering and redaction figures filled in.
func TestProxyPublishesAuditRecord_Success(t *testing.T) {
	nc := inProcNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	auditCh, stop := subscribeAudit(t, nc)
	defer stop()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":50}}`)
	}))
	defer upstream.Close()

	srv := New(pub, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	srv.budget = newBudgetChecker(nil) // no ledger ⇒ allow, no round trip
	if _, err := srv.Start(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	resp, err := http.Post(srv.BaseURL()+"/anthropic/pane-audit-1/v1/messages",
		"application/json", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	rec := recvAudit(t, auditCh)
	if rec.PaneID != "pane-audit-1" {
		t.Errorf("PaneID = %q, want pane-audit-1", rec.PaneID)
	}
	if rec.Dialect != "anthropic" {
		t.Errorf("Dialect = %q, want anthropic", rec.Dialect)
	}
	if rec.Endpoint != "/v1/messages" {
		t.Errorf("Endpoint = %q, want /v1/messages", rec.Endpoint)
	}
	if rec.BudgetState != msg.ProxyBudgetOK {
		t.Errorf("BudgetState = %q, want %q", rec.BudgetState, msg.ProxyBudgetOK)
	}
	if rec.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", rec.Status)
	}
	if rec.InTokens != 100 || rec.OutTokens != 50 {
		t.Errorf("tokens = %d/%d, want 100/50", rec.InTokens, rec.OutTokens)
	}
	if rec.Model != "claude-opus-4-8" {
		t.Errorf("Model = %q, want claude-opus-4-8", rec.Model)
	}
	if rec.TS.IsZero() {
		t.Error("TS is zero")
	}
}

// TestProxyPublishesAuditRecord_BudgetExceeded proves a refusal is audited too —
// governance has to see the requests it blocked, not only the ones it forwarded.
func TestProxyPublishesAuditRecord_BudgetExceeded(t *testing.T) {
	nc := inProcNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	auditCh, stop := subscribeAudit(t, nc)
	defer stop()

	var upstreamHit bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	srv := New(pub, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	// Inject a ledger that says "over ceiling" without a broker.
	srv.budget = newBudgetChecker(nil)
	srv.budget.request = func(string, interface{}, time.Duration) (interface{}, error) {
		return &msg.MsgUsageCheckReply{PaneID: "pane-over", SpentTokens: 5000, CeilingTokens: 1000, Ok: false}, nil
	}
	if _, err := srv.Start(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	resp, err := http.Post(srv.BaseURL()+"/anthropic/pane-over/v1/messages",
		"application/json", strings.NewReader(`{"model":"claude-opus-4-8"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if upstreamHit {
		t.Fatal("upstream was contacted on a budget-refused request")
	}

	rec := recvAudit(t, auditCh)
	if rec.BudgetState != msg.ProxyBudgetExceeded {
		t.Errorf("BudgetState = %q, want %q", rec.BudgetState, msg.ProxyBudgetExceeded)
	}
	if rec.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429", rec.Status)
	}
	if rec.Endpoint != "(budget)" {
		t.Errorf("Endpoint = %q, want (budget)", rec.Endpoint)
	}
}

// TestWriteAuditBody covers the audit_content body sink: off by default, inert
// without a dir, never escapes its directory, and writes the bytes it was given
// (which ServeHTTP has already redacted) verbatim.
func TestWriteAuditBody(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0)
	body := []byte(`{"model":"claude","system":"key is sk_live_SNAT_abc"}`)

	t.Run("off by default writes nothing", func(t *testing.T) {
		dir := t.TempDir()
		s := &Server{auditContent: false, auditDir: dir}
		s.writeAuditBody("pane-1", "anthropic", ts, body)
		if n := countFiles(t, dir); n != 0 {
			t.Fatalf("wrote %d files with audit_content off", n)
		}
	})

	t.Run("no dir is inert even when on", func(t *testing.T) {
		s := &Server{auditContent: true, auditDir: ""}
		s.writeAuditBody("pane-1", "anthropic", ts, body) // must not panic
	})

	t.Run("on writes the redacted body verbatim", func(t *testing.T) {
		dir := t.TempDir()
		s := &Server{auditContent: true, auditDir: dir}
		s.writeAuditBody("pane-1", "anthropic", ts, body)
		files := listFiles(t, dir)
		if len(files) != 1 {
			t.Fatalf("wrote %d files, want 1", len(files))
		}
		got, _ := os.ReadFile(filepath.Join(dir, files[0]))
		if string(got) != string(body) {
			t.Fatalf("stored body = %q, want %q", got, body)
		}
	})

	t.Run("hostile paneID cannot traverse out of the dir", func(t *testing.T) {
		dir := t.TempDir()
		s := &Server{auditContent: true, auditDir: dir}
		s.writeAuditBody("../../etc/pwn", "anthropic", ts, body)
		// The escape target must not exist; the file must be inside dir.
		if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dir)), "etc", "pwn")); err == nil {
			t.Fatal("audit body escaped the audit directory")
		}
		if countFiles(t, dir) != 1 {
			t.Fatalf("expected exactly one file inside the audit dir")
		}
	})
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func countFiles(t *testing.T, dir string) int { return len(listFiles(t, dir)) }
