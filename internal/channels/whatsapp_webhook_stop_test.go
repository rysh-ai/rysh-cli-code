// SPDX-License-Identifier: Apache-2.0

package channels

// F-STOP (WhatsApp webhook mode), security-lead Q2 (ledger 85): Stop must not
// return while the webhook server still holds a request, and a POST that
// arrives once Stop has begun must not be answered 200 and dropped: it gets a
// 503, so Meta retries it, and the next start handles it.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func directWACfg(t *testing.T) msg.ChannelConfig {
	t.Helper()
	return msg.ChannelConfig{APIKey: "TOKEN", Phone: "PN123", AppSecret: e25WAFakeSecret,
		WebhookPort: strconv.Itoa(freePort(t))}
}

func postSample(w *WhatsAppAdapter) int {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(sampleWAPayload))
	req.Header.Set("X-Hub-Signature-256", e25WASign(sampleWAPayload, e25WAFakeSecret))
	w.handleInbound(rec, req)
	return rec.Code
}

func sampleWAID(t *testing.T) string {
	t.Helper()
	ims := parseWhatsAppInbound([]byte(sampleWAPayload))
	if len(ims) == 0 || ims[0].Metadata["message_id"] == "" {
		t.Fatal("sampleWAPayload has no message id")
	}
	return ims[0].Metadata["message_id"]
}

func TestWhatsAppWebhookStopWaitsForTheServer(t *testing.T) {
	t.Chdir(t.TempDir())
	old := whatsAppShutdownGrace
	whatsAppShutdownGrace = 200 * time.Millisecond
	t.Cleanup(func() { whatsAppShutdownGrace = old })

	w := NewWhatsAppAdapter(directWACfg(t))
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// A request in flight: the headers and part of a body that never completes.
	c, err := net.Dial("tcp", w.listenAddr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := io.WriteString(c, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\nX-Hub-Signature-256: sha256=00\r\n\r\npartial"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the handler is reading the body

	done := make(chan struct{})
	go func() { _ = w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked on a request that never completes")
	}
	// The server must already have hung up: a read ends at once (EOF or a
	// reset) instead of timing out on an open connection.
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, err = io.ReadAll(c)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("Stop returned while the webhook server still held a request in flight")
	}
}

func TestWhatsAppWebhookAnswers503OnceStopping(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(msg.ChannelConfig{AppSecret: e25WAFakeSecret})
	_ = w.Stop() // Stop has begun (and finished): nothing is listening any more

	if code := postSample(w); code != http.StatusServiceUnavailable {
		t.Errorf("a POST once stopping got %d; want 503 so Meta retries it", code)
	}
	select {
	case im := <-w.InboundCh():
		t.Fatalf("a POST once stopping was dispatched: %q", im.Metadata["message_id"])
	default:
	}
	if !w.markHandled(sampleWAID(t)) {
		t.Error("the message was marked handled by a stopped adapter; Meta's retry would be dropped as a duplicate")
	}
}

// No-regression guard: a restarted adapter handles POSTs again.
func TestWhatsAppWebhookRestartAcceptsAgain(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(directWACfg(t))
	_ = w.Stop()
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = w.Stop() })
	if code := postSample(w); code != http.StatusOK {
		t.Fatalf("a POST after a restart got %d, want 200", code)
	}
	select {
	case <-w.InboundCh():
	default:
		t.Fatal("a POST after a restart was not dispatched")
	}
}
