package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"
)

// TestWireTest_PlantedSecretNeverLeaves is the design 001 proof: a live-shaped
// Stripe key planted in a request body must NOT appear in the bytes the proxy
// forwards upstream — it is replaced by a synthetic SecretNAT token.
func TestWireTest_PlantedSecretNeverLeaves(t *testing.T) {
	var captured string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"claude-opus-4-8","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":0}}`)
	}))
	defer upstream.Close()

	mgr, err := secretnat.NewManager(secretnat.Options{Enabled: true, Mode: secretnat.ModeSemantic})
	if err != nil {
		t.Fatalf("secretnat manager: %v", err)
	}

	srv := New(nil, mgr, nil, map[string]string{"anthropic": upstream.URL}, false)
	if _, err := srv.Start(""); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer srv.Stop()

	if Endpoint() != srv.BaseURL() {
		t.Fatalf("endpoint = %q, want %q", Endpoint(), srv.BaseURL())
	}

	const planted = "sk_live_ABCD1234abcd5678EFGH9012"
	reqBody := `{"model":"claude-opus-4-8","system":"remember my key is ` + planted +
		`","messages":[{"role":"user","content":"hello"}]}`

	resp, err := http.Post(srv.BaseURL()+"/anthropic/pane-1/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// The security assertion: the raw secret never reached the upstream.
	if strings.Contains(captured, planted) {
		t.Fatalf("SECURITY FAILURE: planted secret leaked upstream:\n%s", captured)
	}
	if !strings.Contains(captured, "sk_live_SNAT") {
		t.Fatalf("expected synthetic token upstream, got:\n%s", captured)
	}
	// Response streamed back intact.
	if !strings.Contains(string(respBody), "output_tokens") {
		t.Fatalf("response not streamed back: %s", respBody)
	}

	// Audit + usage extraction.
	audits := srv.RecentAudits(10)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	a := audits[0]
	if a.RedactionHits < 1 {
		t.Fatalf("redaction hits = %d, want >=1", a.RedactionHits)
	}
	if a.InTokens != 100 || a.OutTokens != 50 {
		t.Fatalf("usage = %d/%d, want 100/50", a.InTokens, a.OutTokens)
	}
	if a.Model != "claude-opus-4-8" {
		t.Fatalf("model = %q, want claude-opus-4-8", a.Model)
	}
}

func TestStopClearsEndpoint(t *testing.T) {
	srv := New(nil, nil, nil, nil, false)
	if _, err := srv.Start(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	if Endpoint() == "" {
		t.Fatal("endpoint empty after start")
	}
	srv.Stop()
	if Endpoint() != "" {
		t.Fatalf("endpoint = %q after stop, want empty", Endpoint())
	}
}

// TestStartPrivate_DoesNotPublish covers the half of probe isolation that lives
// in this package: a private server serves traffic but never touches the
// process globals a pane's env injection reads.
func TestStartPrivate_DoesNotPublish(t *testing.T) {
	live := New(nil, nil, nil, nil, false)
	if _, err := live.Start(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer live.Stop()

	probe := New(nil, nil, nil, nil, false)
	if _, err := probe.StartPrivate(""); err != nil {
		t.Fatalf("start private: %v", err)
	}
	if probe.BaseURL() == "" {
		t.Fatal("a private server must still bind and report its own base URL")
	}
	if Endpoint() != live.BaseURL() || Current() != live {
		t.Fatalf("StartPrivate published itself: endpoint=%q current=%v",
			Endpoint(), Current())
	}

	// And the deferred Stop of a private server must not unpublish the live one.
	probe.Stop()
	if Endpoint() != live.BaseURL() || Current() != live {
		t.Fatalf("a private Stop cleared the live endpoint: endpoint=%q current=%v",
			Endpoint(), Current())
	}
}

// TestStop_OnlyClearsItsOwnPublication guards the restart ordering: a stale
// Stop on a superseded server must not blank the endpoint the new one just
// published.
func TestStop_OnlyClearsItsOwnPublication(t *testing.T) {
	old := New(nil, nil, nil, nil, false)
	if _, err := old.Start(""); err != nil {
		t.Fatalf("start old: %v", err)
	}
	fresh := New(nil, nil, nil, nil, false)
	if _, err := fresh.Start(""); err != nil {
		t.Fatalf("start fresh: %v", err)
	}
	defer fresh.Stop()

	old.Stop()
	if Endpoint() != fresh.BaseURL() || Current() != fresh {
		t.Fatalf("stopping the superseded server cleared the live endpoint: %q", Endpoint())
	}
}

func TestSplitPath(t *testing.T) {
	d, p, rest := splitPath("/anthropic/pane-1/v1/messages")
	if d != "anthropic" || p != "pane-1" || rest != "v1/messages" {
		t.Fatalf("got %q/%q/%q", d, p, rest)
	}
}

func TestParseUsage_SSE(t *testing.T) {
	// input from message_start (first), output from message_delta (last).
	sse := `event: message_start
data: {"type":"message_start","message":{"model":"claude-sonnet-4","usage":{"input_tokens":200,"output_tokens":1,"cache_read_input_tokens":10}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":321}}
`
	in, out, cr, _, model := parseUsage(sse)
	if in != 200 || out != 321 || cr != 10 || model != "claude-sonnet-4" {
		t.Fatalf("parseUsage = in%d out%d cr%d model%q", in, out, cr, model)
	}
}
