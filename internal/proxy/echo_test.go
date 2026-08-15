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

	"github.com/nats-io/nats.go"
	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// planted is live-SHAPED but synthetic. Real shape is the point: it is what the
// SecretNAT detector tier has to catch, in a request or in a response.
const plantedKey = "sk_live_ABCD1234abcd5678EFGH9012"

func testSNAT(t *testing.T) *secretnat.Manager {
	t.Helper()
	mgr, err := secretnat.NewManager(secretnat.Options{
		Enabled: true, Mode: secretnat.ModeSemantic,
	})
	if err != nil {
		t.Fatalf("secretnat manager: %v", err)
	}
	return mgr
}

// subscribePaneRysh collects the rysh-output lines published to a pane, which
// is where the proxy's human-facing warnings land.
func subscribePaneRysh(t *testing.T, nc *nats.Conn, paneID string) <-chan string {
	t.Helper()
	ch := make(chan string, 16)
	sub, err := nc.Subscribe(msg.T("pane", paneID, "output", "rysh"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		var wrapped msg.MsgConversationAppend
		if err := json.Unmarshal(env.Payload, &wrapped); err != nil || wrapped.Message == nil {
			return
		}
		ch <- wrapped.Message.Content
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	_ = nc.Flush()
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return ch
}

func waitForLine(t *testing.T, ch <-chan string, contains string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line := <-ch:
			if strings.Contains(line, contains) {
				return line
			}
		case <-deadline:
			t.Fatalf("no pane line containing %q arrived", contains)
			return ""
		}
	}
}

// TestResponseEchoScan_WarnsOnPlaintextSecret is step 8 of design 001 §3's
// pipeline, which was never built: a response that hands a live credential back
// in plaintext is reported into the pane.
func TestResponseEchoScan_WarnsOnPlaintextSecret(t *testing.T) {
	nc := inProcNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	lines := subscribePaneRysh(t, nc, "pane-echo")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// A tool result the model read back, a config file it printed — the
		// shape does not matter, the plaintext credential does.
		_, _ = io.WriteString(w, `{"model":"m","content":[{"type":"text",`+
			`"text":"your key is `+plantedKey+`"}],`+
			`"usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer upstream.Close()

	srv := New(pub, testSNAT(t), nil, map[string]string{"anthropic": upstream.URL}, false)
	srv.budget = newBudgetChecker(nil)
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	resp, err := http.Post(srv.BaseURL()+"/anthropic/pane-echo/v1/messages",
		"application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// The response body itself is forwarded UNCHANGED — 001 §2/§4.3 say
	// responses are not rewritten, and a proxy that silently edited them would
	// break the CLI's own parsing.
	if !strings.Contains(string(body), plantedKey) {
		t.Fatalf("the response was rewritten; it must be forwarded verbatim: %s", body)
	}

	line := waitForLine(t, lines, "echoed")
	if !strings.Contains(line, "1 secret value") {
		t.Errorf("warning does not say what was seen: %q", line)
	}
	if strings.Contains(line, plantedKey) {
		t.Fatalf("SECURITY FAILURE: the warning quoted the secret itself: %q", line)
	}
	// And it is on the request's own audit line, so `##proxy audit` shows it.
	audits := srv.RecentAudits(5)
	if len(audits) != 1 || audits[0].EchoHits != 1 {
		t.Fatalf("echo not recorded on the audit line: %+v", audits)
	}
	if !strings.Contains(audits[0].String(), "echoed") {
		t.Errorf("audit line does not mention the echo: %s", audits[0].String())
	}
}

// TestResponseEchoScan_IgnoresRyshsOwnTokens is why the scan runs through the
// PANE's session rather than a throwaway one. A synthetic token is shaped like
// a live Stripe key on purpose, and the provider echoing one back is CORRECT
// behaviour (001 §4.3) — warning about it would cry wolf on every redacted
// conversation.
func TestResponseEchoScan_IgnoresRyshsOwnTokens(t *testing.T) {
	nc := inProcNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	lines := subscribePaneRysh(t, nc, "pane-token")

	var forwarded string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		forwarded = string(b)
		w.Header().Set("Content-Type", "application/json")
		// Echo the (redacted) request back, exactly as a model quoting its
		// input would.
		_, _ = io.WriteString(w, `{"model":"m","echo":`+quoteJSON(forwarded)+
			`,"usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer upstream.Close()

	srv := New(pub, testSNAT(t), nil, map[string]string{"anthropic": upstream.URL}, false)
	srv.budget = newBudgetChecker(nil)
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	resp, err := http.Post(srv.BaseURL()+"/anthropic/pane-token/v1/messages",
		"application/json",
		strings.NewReader(`{"model":"m","system":"key `+plantedKey+`","messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Non-vacuous: the response really did contain a SNAT token.
	if !strings.Contains(forwarded, "sk_live_SNAT") {
		t.Fatalf("the request was not redacted, so this proves nothing: %s", forwarded)
	}
	audits := srv.RecentAudits(5)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	if audits[0].EchoHits != 0 {
		t.Fatalf("EchoHits = %d — rysh's own stand-in token was reported as an "+
			"echoed secret", audits[0].EchoHits)
	}
	select {
	case line := <-lines:
		if strings.Contains(line, "echoed") {
			t.Fatalf("false-positive echo warning: %q", line)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestAuditContent_EndToEnd is the missing proof for design 001 §4.5's body
// sink. Until now only writeAuditBody was tested, called directly with
// pre-cleaned bytes — so nothing showed that a REAL request carrying a real
// secret cannot land in .rysh/proxy-audit/ in plaintext.
func TestAuditContent_EndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	srv := New(nil, testSNAT(t), nil, map[string]string{"anthropic": upstream.URL}, true)
	srv.SetAuditDir(dir)
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	resp, err := http.Post(srv.BaseURL()+"/anthropic/pane-audit/v1/messages",
		"application/json",
		strings.NewReader(`{"model":"m","system":"my key is `+plantedKey+
			`","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	files := listFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("audit_content wrote %d files, want 1", len(files))
	}
	stored, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatalf("read stored body: %v", err)
	}
	// THE assertion: the sink holds the redacted body, never the original.
	if strings.Contains(string(stored), plantedKey) {
		t.Fatalf("SECURITY FAILURE: audit_content stored the plaintext secret:\n%s", stored)
	}
	if !strings.Contains(string(stored), "sk_live_SNAT") {
		t.Fatalf("stored body is not the redacted one (no SNAT token):\n%s", stored)
	}
	// Non-vacuous: the rest of the request really is there, so the test cannot
	// pass because nothing useful was written.
	if !strings.Contains(string(stored), `"role":"user"`) {
		t.Fatalf("stored body is not the request body:\n%s", stored)
	}
	// The file lives under the configured sink and nowhere else.
	if got := filepath.Dir(filepath.Join(dir, files[0])); got != dir {
		t.Fatalf("body written outside the audit dir: %s", got)
	}
}

// TestAuditContent_OffByDefaultEndToEnd: a real request through a proxy with
// audit_content off must leave no body behind at all.
func TestAuditContent_OffByDefaultEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	srv := New(nil, testSNAT(t), nil, map[string]string{"anthropic": upstream.URL}, false)
	srv.SetAuditDir(dir)
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	resp, err := http.Post(srv.BaseURL()+"/anthropic/pane-audit/v1/messages",
		"application/json", strings.NewReader(`{"system":"`+plantedKey+`"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if n := countFiles(t, dir); n != 0 {
		t.Fatalf("audit_content off still wrote %d file(s)", n)
	}
}
