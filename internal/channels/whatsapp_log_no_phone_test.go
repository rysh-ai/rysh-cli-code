// SPDX-License-Identifier: Apache-2.0

package channels

// security-lead N1 (F-STOP review, ledger 85): the WhatsApp log lines carried
// the contact's phone number ("from", "to", "recipient"). A log is copied into
// bug reports and support threads; the message id is enough to follow one
// message, so no line names the number.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const waLogPhone = "447700900123" // the sender in sampleWAPayload and every case below

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func relayInboundFrom(t *testing.T, id string, ts int64) []byte {
	t.Helper()
	data, err := json.Marshal(relayInbound{ChannelType: "whatsapp", ConnectionID: "conn-1", WorkspaceID: "ws-1",
		SenderID: waLogPhone, MessageID: id, Content: "hello", Timestamp: ts})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func noPhoneInLog(t *testing.T, buf *bytes.Buffer, wantLines ...string) {
	t.Helper()
	log := buf.String()
	for _, w := range wantLines {
		if !strings.Contains(log, w) {
			t.Fatalf("the case did not reach the log line %q; log:\n%s", w, log)
		}
	}
	if n := strings.Count(log, waLogPhone); n != 0 {
		t.Errorf("%d log line field(s) carry the contact's phone number:\n%s", n, log)
	}
}

func TestWhatsAppRelayLogsCarryNoPhone(t *testing.T) {
	t.Chdir(t.TempDir())
	buf := captureSlog(t)
	w := NewWhatsAppAdapter(relayCfg())
	now := time.Now().Unix()

	w.handleRelayInbound(relayInboundFrom(t, "wamid.a", now), false)
	w.handleRelayInbound(relayInboundFrom(t, "wamid.a", now), false) // duplicate
	w.handleRelayInbound(relayInboundFrom(t, "wamid.old", now-30*24*3600), true)
	w.mu.Lock()
	w.relayRefused = true
	w.mu.Unlock()
	w.handleRelayInbound(relayInboundFrom(t, "wamid.held", now), false)
	w.mu.Lock()
	w.relayRefused = false
	w.inbound = make(chan InboundMessage) // no reader: the buffer is full
	w.mu.Unlock()
	w.handleRelayInbound(relayInboundFrom(t, "wamid.full", now), false)
	st, _ := json.Marshal(relayStatus{MessageID: "wamid.out", RecipientID: waLogPhone, Status: "failed", ErrorCode: 131047})
	w.handleRelayStatus(st)

	noPhoneInLog(t, buf, "ignoring duplicate inbound", "too old to auto-answer", "held by another session",
		"inbound buffer full", "delivery failed")
}

func TestWhatsAppWebhookLogsCarryNoPhone(t *testing.T) {
	t.Chdir(t.TempDir())
	buf := captureSlog(t)
	w := NewWhatsAppAdapter(msg.ChannelConfig{AppSecret: e25WAFakeSecret})
	post := func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(sampleWAPayload))
		req.Header.Set("X-Hub-Signature-256", e25WASign(sampleWAPayload, e25WAFakeSecret))
		w.handleInbound(rec, req)
	}
	post()
	post() // duplicate
	<-w.InboundCh()
	w.mu.Lock()
	w.inbound = make(chan InboundMessage) // no reader: the buffer is full
	w.handled = nil
	w.handledOrder = nil
	w.mu.Unlock()
	post()
	w.handleStatuses([]waStatusUpdate{
		{MessageID: "wamid.out1", RecipientID: waLogPhone, Status: "failed", ErrCode: 131047},
		{MessageID: "wamid.out2", RecipientID: waLogPhone, Status: "delivered"},
	})

	noPhoneInLog(t, buf, "ignoring duplicate inbound", "inbound buffer full", "delivery failed", "delivery status")
}

func TestWhatsAppSendLogsCarryNoPhone(t *testing.T) {
	t.Chdir(t.TempDir())
	buf := captureSlog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(rw, `{"messages":[{"id":"wamid.OUT"}]}`)
	}))
	defer srv.Close()
	w := NewWhatsAppAdapter(msg.ChannelConfig{APIKey: "TOKEN", Phone: "PN123", DefaultTemplate: "hello_again"})
	w.baseURL = srv.URL
	w.connected = true

	// Outside the 24h window: the re-engagement template.
	if err := w.Send(context.Background(), OutboundMessage{RecipientID: waLogPhone, Content: "hi"}); err != nil {
		t.Fatalf("template Send: %v", err)
	}
	// Inside the window: free-form text.
	w.received = append(w.received, WhatsAppMessage{From: waLogPhone, Time: time.Now()})
	if err := w.Send(context.Background(), OutboundMessage{RecipientID: waLogPhone, Content: "hi there"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	noPhoneInLog(t, buf, "session window closed", "template sent", "whatsapp: sent")
}

// security-lead L1 (ledger 89): the window-closed error is logged by callers,
// so its text must not carry the number either.
func TestWhatsAppWindowClosedErrorCarriesNoPhone(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(msg.ChannelConfig{APIKey: "TOKEN", Phone: "PN123"}) // no default_template
	w.connected = true
	err := w.Send(context.Background(), OutboundMessage{RecipientID: waLogPhone, Content: "hi"})
	if err == nil || !strings.Contains(err.Error(), "24h customer-service window is closed") {
		t.Fatalf("Send outside the window = %v; want the window-closed error", err)
	}
	if strings.Contains(err.Error(), waLogPhone) {
		t.Errorf("the window-closed error carries the contact's phone number: %v", err)
	}
}
