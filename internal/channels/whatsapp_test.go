// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const sampleWAPayload = `{
  "object": "whatsapp_business_account",
  "entry": [{
    "id": "BUSINESS_ID",
    "changes": [{
      "field": "messages",
      "value": {
        "messaging_product": "whatsapp",
        "metadata": {"display_phone_number": "15550001111", "phone_number_id": "PN123"},
        "contacts": [{"profile": {"name": "Alice"}, "wa_id": "447700900123"}],
        "messages": [{
          "from": "447700900123",
          "id": "wamid.ABC",
          "timestamp": "1700000000",
          "type": "text",
          "text": {"body": "hello bot"}
        }]
      }
    }]
  }]
}`

func TestParseWhatsAppInbound(t *testing.T) {
	msgs := parseWhatsAppInbound([]byte(sampleWAPayload))
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.SenderID != "447700900123" {
		t.Errorf("SenderID = %q, want 447700900123", m.SenderID)
	}
	if m.SenderName != "Alice" {
		t.Errorf("SenderName = %q, want Alice", m.SenderName)
	}
	if m.Content != "hello bot" {
		t.Errorf("Content = %q, want 'hello bot'", m.Content)
	}
	if m.ThreadID != "447700900123" {
		t.Errorf("ThreadID = %q, want sender phone", m.ThreadID)
	}
	if m.Metadata["phone_number_id"] != "PN123" {
		t.Errorf("phone_number_id metadata = %q, want PN123", m.Metadata["phone_number_id"])
	}
	// Critical: must NOT set metadata["channel"] — the humanoid would use it as
	// the outbound recipient and misroute the reply.
	if _, ok := m.Metadata["channel"]; ok {
		t.Errorf("metadata must not contain a 'channel' key (would misroute outbound)")
	}
}

func TestParseWhatsAppInbound_IgnoresNonText(t *testing.T) {
	// A status-receipt payload (no messages) must yield nothing.
	const statusPayload = `{"entry":[{"changes":[{"value":{"statuses":[{"status":"delivered"}]}}]}]}`
	if msgs := parseWhatsAppInbound([]byte(statusPayload)); len(msgs) != 0 {
		t.Fatalf("expected 0 messages for status payload, got %d", len(msgs))
	}
}

func TestVerifyMetaSignature(t *testing.T) {
	body := []byte(sampleWAPayload)
	secret := "app-secret-xyz"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyMetaSignature(body, good, secret) {
		t.Error("valid signature rejected")
	}
	if verifyMetaSignature(body, "sha256=deadbeef", secret) {
		t.Error("invalid signature accepted")
	}
	if verifyMetaSignature(body, "", secret) {
		t.Error("empty signature header accepted")
	}
}

func TestWhatsAppVerifyHandshake(t *testing.T) {
	w := NewWhatsAppAdapter(msg.ChannelConfig{VerifyToken: "tok123"})

	// Correct token echoes the challenge.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/?hub.mode=subscribe&hub.verify_token=tok123&hub.challenge=NONCE", nil)
	w.handleVerify(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "NONCE" {
		t.Fatalf("verify: code=%d body=%q, want 200/NONCE", rec.Code, rec.Body.String())
	}

	// Wrong token is rejected.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet,
		"/?hub.mode=subscribe&hub.verify_token=WRONG&hub.challenge=NONCE", nil)
	w.handleVerify(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("verify with wrong token: code=%d, want 403", rec.Code)
	}
}

func TestWhatsAppInboundHandlerEnqueues(t *testing.T) {
	w := NewWhatsAppAdapter(msg.ChannelConfig{}) // no app secret -> signature check skipped
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(sampleWAPayload))
	w.handleInbound(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("inbound POST code=%d, want 200", rec.Code)
	}
	select {
	case m := <-w.InboundCh():
		if m.Content != "hello bot" {
			t.Errorf("enqueued Content = %q, want 'hello bot'", m.Content)
		}
	default:
		t.Fatal("expected a message on the inbound channel")
	}
}

func TestWhatsAppInboundRejectsBadSignature(t *testing.T) {
	w := NewWhatsAppAdapter(msg.ChannelConfig{AppSecret: "s3cr3t"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(sampleWAPayload))
	req.Header.Set("X-Hub-Signature-256", "sha256=bogus")
	w.handleInbound(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("inbound with bad signature: code=%d, want 401", rec.Code)
	}
}

func TestWhatsAppSend(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, `{"messages":[{"id":"wamid.OUT"}]}`)
	}))
	defer srv.Close()

	w := NewWhatsAppAdapter(msg.ChannelConfig{APIKey: "TOKEN", Phone: "PN123"})
	w.baseURL = srv.URL
	w.connected = true
	// GA hardening routes free-form text only inside the 24h session window, so
	// record a fresh inbound from the recipient to open it (typical reply flow).
	w.received = append(w.received, WhatsAppMessage{From: "447700900123", Time: time.Now()})

	if err := w.Send(context.Background(), OutboundMessage{RecipientID: "447700900123", Content: "hi there"}); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if gotAuth != "Bearer TOKEN" {
		t.Errorf("Authorization = %q, want 'Bearer TOKEN'", gotAuth)
	}
	if gotPath != "/"+defaultWhatsAppGraphVersion+"/PN123/messages" {
		t.Errorf("path = %q, want /%s/PN123/messages", gotPath, defaultWhatsAppGraphVersion)
	}
	var payload waSendPayload
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if payload.To != "447700900123" || payload.Text.Body != "hi there" || payload.MessagingProduct != "whatsapp" {
		t.Errorf("unexpected payload: %+v", payload)
	}
}

func TestWhatsAppSendNotConnected(t *testing.T) {
	w := NewWhatsAppAdapter(msg.ChannelConfig{APIKey: "TOKEN", Phone: "PN123"})
	if err := w.Send(context.Background(), OutboundMessage{RecipientID: "x", Content: "y"}); err == nil {
		t.Fatal("expected error when not connected")
	}
}

func TestWhatsAppConfigParsing(t *testing.T) {
	// webhook_port and graph_version arrive as (already secret-expanded) strings.
	w := NewWhatsAppAdapter(msg.ChannelConfig{WebhookPort: "23310", GraphVersion: "v21.0"})
	if w.port != 23310 {
		t.Errorf("port = %d, want 23310", w.port)
	}
	if w.graphVer != "v21.0" {
		t.Errorf("graphVer = %q, want v21.0", w.graphVer)
	}

	// Empty / invalid values fall back to defaults.
	def := NewWhatsAppAdapter(msg.ChannelConfig{WebhookPort: "", GraphVersion: ""})
	if def.port != defaultWhatsAppWebhookPort {
		t.Errorf("empty port = %d, want default %d", def.port, defaultWhatsAppWebhookPort)
	}
	if def.graphVer != defaultWhatsAppGraphVersion {
		t.Errorf("empty graphVer = %q, want default %q", def.graphVer, defaultWhatsAppGraphVersion)
	}
	if bad := NewWhatsAppAdapter(msg.ChannelConfig{WebhookPort: "not-a-number"}); bad.port != defaultWhatsAppWebhookPort {
		t.Errorf("invalid port = %d, want default %d", bad.port, defaultWhatsAppWebhookPort)
	}
}

func TestSplitMessage(t *testing.T) {
	if got := splitMessage("short", 100); len(got) != 1 || got[0] != "short" {
		t.Errorf("short message should be 1 chunk, got %v", got)
	}
	long := strings.Repeat("a", 50) + "\n" + strings.Repeat("b", 50)
	chunks := splitMessage(long, 60)
	if len(chunks) < 2 {
		t.Errorf("expected long message to split, got %d chunks", len(chunks))
	}
	if strings.Join(chunks, "") != long {
		t.Error("split chunks must reassemble to the original")
	}
}
