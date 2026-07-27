package channels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// seedInbound plants an inbound-message record with a controlled timestamp so
// tests can position a recipient precisely around the 24h window edge without
// touching the persistence path (storeReceived would write channel-state files).
func seedInbound(w *WhatsAppAdapter, from string, at time.Time) {
	w.mu.Lock()
	w.received = append(w.received, WhatsAppMessage{From: from, Time: at})
	w.mu.Unlock()
}

func TestWhatsAppInSessionWindow(t *testing.T) {
	w := NewWhatsAppAdapter(msg.ChannelConfig{})
	now := time.Now()
	seedInbound(w, "inwindow", now.Add(-23*time.Hour-59*time.Minute))
	seedInbound(w, "expired", now.Add(-24*time.Hour-1*time.Minute))
	// A sender with an old message followed by a fresh one must count as
	// in-window: only the MOST RECENT inbound matters.
	seedInbound(w, "reengaged", now.Add(-48*time.Hour))
	seedInbound(w, "reengaged", now.Add(-1*time.Hour))

	if !w.inSessionWindow("inwindow") {
		t.Error("23h59m-old inbound should be in-window")
	}
	if w.inSessionWindow("expired") {
		t.Error("24h01m-old inbound should be out-of-window")
	}
	if !w.inSessionWindow("reengaged") {
		t.Error("a fresh inbound after an old one should reopen the window")
	}
	if w.inSessionWindow("never-seen") {
		t.Error("a recipient with no recorded inbound must be out-of-window")
	}
}

// newFakeGraph returns an adapter pointed at an httptest Graph endpoint that
// records each request body and replies with the queued status codes (200 for
// any request beyond the queue).
func newFakeGraph(t *testing.T, cfg msg.ChannelConfig, statuses ...int) (*WhatsAppAdapter, *[]string) {
	t.Helper()
	var bodies []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		code := http.StatusOK
		if n < len(statuses) {
			code = statuses[n]
		}
		n++
		rw.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = io.WriteString(rw, `{"messages":[{"id":"wamid.OUT"}]}`)
		} else {
			_, _ = io.WriteString(rw, `{"error":{"message":"limited","code":4}}`)
		}
	}))
	t.Cleanup(srv.Close)

	if cfg.APIKey == "" {
		cfg.APIKey = "TOKEN"
	}
	if cfg.Phone == "" {
		cfg.Phone = "PN-GA-TEST"
	}
	w := NewWhatsAppAdapter(cfg)
	w.baseURL = srv.URL
	w.connected = true
	w.retryBaseDelay = time.Millisecond // keep backoff tests fast
	return w, &bodies
}

func TestWhatsAppSendRouting_InWindowFreeForm(t *testing.T) {
	w, bodies := newFakeGraph(t, msg.ChannelConfig{DefaultTemplate: "reengage_hello"})
	seedInbound(w, "447700900123", time.Now().Add(-time.Hour))

	if err := w.Send(context.Background(), OutboundMessage{RecipientID: "447700900123", Content: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("want 1 request, got %d", len(*bodies))
	}
	var p waSendPayload
	if err := json.Unmarshal([]byte((*bodies)[0]), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "text" || p.Text.Body != "hello" {
		t.Errorf("in-window send must be free-form text, got: %s", (*bodies)[0])
	}
}

func TestWhatsAppSendRouting_OutOfWindowUsesTemplate(t *testing.T) {
	w, bodies := newFakeGraph(t, msg.ChannelConfig{DefaultTemplate: "reengage_hello", TemplateLang: "en_GB"})
	// No inbound seeded: the recipient has never messaged us -> out-of-window.

	if err := w.Send(context.Background(), OutboundMessage{RecipientID: "447700900999", Content: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(*bodies) != 1 {
		t.Fatalf("want 1 request, got %d", len(*bodies))
	}
	var p waTemplatePayload
	if err := json.Unmarshal([]byte((*bodies)[0]), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "template" || p.Template.Name != "reengage_hello" || p.Template.Language.Code != "en_GB" {
		t.Errorf("out-of-window send must use the configured template, got: %s", (*bodies)[0])
	}
	// The default template takes no params, so components must be absent.
	if strings.Contains((*bodies)[0], "components") {
		t.Errorf("components must be omitted for a parameterless template: %s", (*bodies)[0])
	}
}

func TestWhatsAppSendRouting_OutOfWindowNoTemplateFailsHonestly(t *testing.T) {
	w, bodies := newFakeGraph(t, msg.ChannelConfig{}) // no default_template

	err := w.Send(context.Background(), OutboundMessage{RecipientID: "447700900999", Content: "hello"})
	if err == nil {
		t.Fatal("expected an error for an out-of-window send without a template")
	}
	// The guardrail must explain the 24h rule and name the template requirement,
	// and must not have attempted a doomed HTTP send.
	if !strings.Contains(err.Error(), "24h") || !strings.Contains(err.Error(), "template") {
		t.Errorf("error should name the 24h rule and templates: %v", err)
	}
	if len(*bodies) != 0 {
		t.Errorf("no HTTP request should be made, got %d", len(*bodies))
	}
}

func TestWhatsAppSendTemplatePayloadShape(t *testing.T) {
	w, bodies := newFakeGraph(t, msg.ChannelConfig{})

	// With body params: a single body component with positional text parameters.
	if err := w.SendTemplate(context.Background(), "447700900123", "order_update", "en_US", []string{"A-42", "shipped"}); err != nil {
		t.Fatalf("SendTemplate: %v", err)
	}
	var p waTemplatePayload
	if err := json.Unmarshal([]byte((*bodies)[0]), &p); err != nil {
		t.Fatal(err)
	}
	if p.MessagingProduct != "whatsapp" || p.To != "447700900123" || p.Type != "template" {
		t.Errorf("envelope wrong: %s", (*bodies)[0])
	}
	if p.Template.Name != "order_update" || p.Template.Language.Code != "en_US" {
		t.Errorf("template name/language wrong: %s", (*bodies)[0])
	}
	if len(p.Template.Components) != 1 || p.Template.Components[0].Type != "body" {
		t.Fatalf("want one body component: %s", (*bodies)[0])
	}
	params := p.Template.Components[0].Parameters
	if len(params) != 2 || params[0].Text != "A-42" || params[1].Type != "text" {
		t.Errorf("body parameters wrong: %s", (*bodies)[0])
	}

	// Without params: components omitted; empty language falls back to en_US.
	if err := w.SendTemplate(context.Background(), "447700900123", "hello_world", "", nil); err != nil {
		t.Fatalf("SendTemplate no-params: %v", err)
	}
	if strings.Contains((*bodies)[1], "components") {
		t.Errorf("components must be omitted when there are no params: %s", (*bodies)[1])
	}
	if !strings.Contains((*bodies)[1], `"code":"en_US"`) {
		t.Errorf("empty language must default to en_US: %s", (*bodies)[1])
	}

	// Argument validation.
	if err := w.SendTemplate(context.Background(), "", "hello_world", "", nil); err == nil {
		t.Error("empty recipient must error")
	}
	if err := w.SendTemplate(context.Background(), "447700900123", "", "", nil); err == nil {
		t.Error("empty template name must error")
	}
}

// sampleWAStatusPayload is a recorded-shape webhook body carrying delivery
// statuses (no messages[]): delivered, read, and a failure with error details.
const sampleWAStatusPayload = `{
  "object": "whatsapp_business_account",
  "entry": [{
    "changes": [{
      "field": "messages",
      "value": {
        "messaging_product": "whatsapp",
        "metadata": {"phone_number_id": "PN123"},
        "statuses": [
          {"id": "wamid.S1", "status": "delivered", "timestamp": "1700000001", "recipient_id": "447700900123"},
          {"id": "wamid.S2", "status": "read", "timestamp": "1700000002", "recipient_id": "447700900123"},
          {"id": "wamid.S3", "status": "failed", "timestamp": "1700000003", "recipient_id": "447700900456",
           "errors": [{"code": 131047, "title": "Re-engagement message"}]}
        ]
      }
    }]
  }]
}`

func TestParseWhatsAppStatuses(t *testing.T) {
	sts := parseWhatsAppStatuses([]byte(sampleWAStatusPayload))
	if len(sts) != 3 {
		t.Fatalf("want 3 statuses, got %d", len(sts))
	}
	if sts[0].Status != "delivered" || sts[0].MessageID != "wamid.S1" || sts[0].ErrCode != 0 {
		t.Errorf("delivered status parsed wrong: %+v", sts[0])
	}
	if sts[1].Status != "read" || sts[1].RecipientID != "447700900123" {
		t.Errorf("read status parsed wrong: %+v", sts[1])
	}
	if sts[2].Status != "failed" || sts[2].ErrCode != 131047 || sts[2].ErrTitle != "Re-engagement message" {
		t.Errorf("failed status parsed wrong: %+v", sts[2])
	}

	// A messages-only payload carries no statuses.
	if got := parseWhatsAppStatuses([]byte(sampleWAPayload)); len(got) != 0 {
		t.Errorf("messages payload should yield no statuses, got %d", len(got))
	}
	// An error with no title falls back to the known-code hint.
	const untitled = `{"entry":[{"changes":[{"value":{"statuses":[
	  {"id":"wamid.S4","status":"failed","recipient_id":"447700900456","errors":[{"code":131026}]}]}}]}]}`
	sts = parseWhatsAppStatuses([]byte(untitled))
	if len(sts) != 1 || !strings.Contains(sts[0].ErrTitle, "undeliverable") {
		t.Errorf("expected the 131026 hint, got %+v", sts)
	}
}

func TestWhatsAppStatusWebhook_LogsFailuresNotInbound(t *testing.T) {
	w := NewWhatsAppAdapter(msg.ChannelConfig{}) // no app secret -> signature check skipped
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(sampleWAStatusPayload))
	w.handleInbound(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status webhook code=%d, want 200", rec.Code)
	}

	// Statuses are receipts, not conversation: nothing may reach InboundCh.
	select {
	case m := <-w.InboundCh():
		t.Fatalf("statuses must not become InboundMessages, got %+v", m)
	default:
	}

	// The failure must surface in Status().Details with its error code.
	details := w.Status().Details
	if !strings.Contains(details, "131047") || !strings.Contains(details, "447700900456") {
		t.Errorf("Status details should surface the latest delivery failure, got %q", details)
	}
}

func TestWhatsAppDeliveryFailureRingCapped(t *testing.T) {
	w := NewWhatsAppAdapter(msg.ChannelConfig{})
	for i := 0; i < whatsAppFailureCap+5; i++ {
		w.recordDeliveryFailure(waStatusUpdate{RecipientID: "447700900456", Status: "failed", ErrCode: 131026})
	}
	w.mu.RLock()
	got := len(w.failures)
	w.mu.RUnlock()
	if got != whatsAppFailureCap {
		t.Errorf("failure ring: want cap %d, got %d", whatsAppFailureCap, got)
	}
}

func TestWhatsAppRateLimitBackoffAndRecovery(t *testing.T) {
	// Two 429s, then success: the send must retry through the backoff and land.
	w, bodies := newFakeGraph(t, msg.ChannelConfig{}, http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusOK)
	seedInbound(w, "447700900123", time.Now().Add(-time.Minute))

	if err := w.Send(context.Background(), OutboundMessage{RecipientID: "447700900123", Content: "hi"}); err != nil {
		t.Fatalf("Send should succeed after retries: %v", err)
	}
	if len(*bodies) != 3 {
		t.Errorf("want 3 attempts, got %d", len(*bodies))
	}
	if !strings.Contains(w.Status().Details, "rate limited") {
		t.Errorf("Status details should record rate limiting, got %q", w.Status().Details)
	}
}

func TestWhatsAppRateLimitGivesUpAfterMaxAttempts(t *testing.T) {
	// Every attempt 429s: the send must stop after whatsAppSendMaxAttempts and
	// return a rate-limit error instead of retrying forever.
	codes := make([]int, whatsAppSendMaxAttempts+2)
	for i := range codes {
		codes[i] = http.StatusTooManyRequests
	}
	w, bodies := newFakeGraph(t, msg.ChannelConfig{}, codes...)
	seedInbound(w, "447700900123", time.Now().Add(-time.Minute))

	err := w.Send(context.Background(), OutboundMessage{RecipientID: "447700900123", Content: "hi"})
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("expected a rate-limited error, got %v", err)
	}
	if len(*bodies) != whatsAppSendMaxAttempts {
		t.Errorf("want %d attempts, got %d", whatsAppSendMaxAttempts, len(*bodies))
	}
}

func TestWhatsAppGraphErrorCode130472Retries(t *testing.T) {
	// Graph reports tier throttling with error code 130472 on a non-429 status;
	// that must be treated as rate limiting (retry), and a later 200 succeeds.
	var bodies []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		n++
		if n == 1 {
			rw.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(rw, `{"error":{"message":"Spam rate limit hit","code":130472}}`)
			return
		}
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, `{"messages":[{"id":"wamid.OUT"}]}`)
	}))
	defer srv.Close()

	w := NewWhatsAppAdapter(msg.ChannelConfig{APIKey: "TOKEN", Phone: "PN-GA-TEST"})
	w.baseURL = srv.URL
	w.connected = true
	w.retryBaseDelay = time.Millisecond
	seedInbound(w, "447700900123", time.Now().Add(-time.Minute))

	if err := w.Send(context.Background(), OutboundMessage{RecipientID: "447700900123", Content: "hi"}); err != nil {
		t.Fatalf("Send should retry through 130472: %v", err)
	}
	if len(bodies) != 2 {
		t.Errorf("want 2 attempts, got %d", len(bodies))
	}

	// A plain non-retryable error (401) must NOT be retried.
	w2, b2 := newFakeGraph(t, msg.ChannelConfig{}, http.StatusUnauthorized)
	seedInbound(w2, "447700900123", time.Now().Add(-time.Minute))
	if err := w2.Send(context.Background(), OutboundMessage{RecipientID: "447700900123", Content: "hi"}); err == nil {
		t.Fatal("401 should fail")
	}
	if len(*b2) != 1 {
		t.Errorf("401 must not retry: want 1 attempt, got %d", len(*b2))
	}
}
