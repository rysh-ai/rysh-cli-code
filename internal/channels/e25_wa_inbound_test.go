// SPDX-License-Identifier: Apache-2.0

package channels

// e25_wa_inbound_test.go — WhatsApp inbound fails closed (E25, root security
// task 2026-10-28). A direct-mode adapter must not accept an unsigned webhook
// POST: Start refuses without app_secret, the handler refuses unsigned or
// wrongly-signed bodies even if Start was bypassed, and the listener binds
// loopback unless a non-loopback host is configured on purpose.
//
// Every secret here is a synthetic fixture, never a real credential.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const e25WAFakeSecret = "e25-synthetic-app-secret"

func e25WASign(body, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func e25WADirectConfig() msg.ChannelConfig {
	return msg.ChannelConfig{
		APIKey:      "e25-synthetic-graph-token",
		Phone:       "100000000000001",
		VerifyToken: "e25-synthetic-verify",
		WebhookPort: "0", // any free port
	}
}

// (a) Start refuses a direct-mode config without app_secret, before any
// listener is bound, and names the field.
func TestE25WhatsAppStartRefusesWithoutAppSecret(t *testing.T) {
	t.Chdir(t.TempDir()) // the adapter persists its store under the cwd
	for name, secret := range map[string]string{
		"empty":      "",
		"blank":      "   ",
		"unresolved": "${WHATSAPP_APP_SECRET}",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := e25WADirectConfig()
			cfg.AppSecret = secret
			w := NewWhatsAppAdapter(cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := w.Start(ctx)
			if err == nil {
				_ = w.Stop()
				t.Fatal("Start succeeded without app_secret; want a refusal")
			}
			if !strings.Contains(err.Error(), "app_secret") {
				t.Errorf("err = %v, want it to name app_secret", err)
			}
			if w.server != nil {
				t.Error("a webhook server was created before the refusal")
			}
		})
	}
}

// Relay mode is unchanged: rysh-server owns the webhook and verifies it, so
// the local adapter needs no app_secret there. Checked through the config
// gate only (startRelay itself dials the server).
func TestE25WhatsAppRelayNeedsNoAppSecret(t *testing.T) {
	cfg := msg.ChannelConfig{Relay: true}
	if err := whatsAppDirectConfigError(cfg); err != nil {
		t.Fatalf("relay config refused: %v", err)
	}
	cfg = e25WADirectConfig()
	if err := whatsAppDirectConfigError(cfg); err == nil {
		t.Fatal("direct config without app_secret passed the gate")
	}
	cfg.AppSecret = e25WAFakeSecret
	if err := whatsAppDirectConfigError(cfg); err != nil {
		t.Fatalf("complete direct config refused: %v", err)
	}
}

// (a, defence in depth) The handler refuses an unsigned or wrongly-signed
// POST even when no app_secret is configured, and enqueues nothing.
func TestE25WhatsAppInboundRefusesUnsigned(t *testing.T) {
	cases := map[string]struct {
		secret, header string
	}{
		"no secret, no header":  {"", ""},
		"no secret, any header": {"", "sha256=00"},
		"secret, no header":     {e25WAFakeSecret, ""},
		"secret, wrong secret":  {e25WAFakeSecret, e25WASign(sampleWAPayload, "other-synthetic")},
		"secret, malformed sig": {e25WAFakeSecret, "md5=abc"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := NewWhatsAppAdapter(msg.ChannelConfig{AppSecret: c.secret})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(sampleWAPayload))
			if c.header != "" {
				req.Header.Set("X-Hub-Signature-256", c.header)
			}
			w.handleInbound(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("code = %d, want 401", rec.Code)
			}
			select {
			case m := <-w.InboundCh():
				t.Errorf("an unsigned POST was enqueued (from %q)", m.SenderID)
			default:
			}
		})
	}
}

// A correctly signed POST is still accepted.
func TestE25WhatsAppInboundAcceptsSigned(t *testing.T) {
	w := NewWhatsAppAdapter(msg.ChannelConfig{AppSecret: e25WAFakeSecret})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(sampleWAPayload))
	req.Header.Set("X-Hub-Signature-256", e25WASign(sampleWAPayload, e25WAFakeSecret))
	w.handleInbound(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	select {
	case <-w.InboundCh():
	default:
		t.Fatal("a signed POST was not enqueued")
	}
}

// (b) With no webhook_bind the listener is on 127.0.0.1, and an unsigned POST
// through the real listener is refused while a signed one is accepted.
func TestE25WhatsAppStartBindsLoopbackByDefault(t *testing.T) {
	t.Chdir(t.TempDir()) // the adapter persists its store under the cwd
	cfg := e25WADirectConfig()
	cfg.AppSecret = e25WAFakeSecret
	w := NewWhatsAppAdapter(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	addr := w.listenAddr()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("listen addr = %q, want 127.0.0.1:<port>", addr)
	}
	if d := w.Status().Details; !strings.Contains(d, addr) {
		t.Errorf("Status details %q do not show the bound address %q", d, addr)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	post := func(sig string) int {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/", strings.NewReader(sampleWAPayload))
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(""); code != http.StatusUnauthorized {
		t.Errorf("unsigned POST over the listener: code = %d, want 401", code)
	}
	if code := post(e25WASign(sampleWAPayload, e25WAFakeSecret)); code != http.StatusOK {
		t.Errorf("signed POST over the listener: code = %d, want 200", code)
	}
}

// (b) A non-loopback bind happens only when webhook_bind names it; a value
// that is not an IP or localhost is refused rather than guessed.
func TestE25WhatsAppBindHost(t *testing.T) {
	ok := map[string]string{
		"":          "127.0.0.1",
		"  ":        "127.0.0.1",
		"127.0.0.1": "127.0.0.1",
		"localhost": "localhost",
		"::1":       "::1",
		"0.0.0.0":   "0.0.0.0",
		"10.1.2.3":  "10.1.2.3",
	}
	for in, want := range ok {
		got, err := whatsAppBindHost(msg.ChannelConfig{WebhookBind: in})
		if err != nil || got != want {
			t.Errorf("whatsAppBindHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"example.com", "0.0.0.0:80", "${BIND}", "1.2.3"} {
		if _, err := whatsAppBindHost(msg.ChannelConfig{WebhookBind: bad}); err == nil {
			t.Errorf("whatsAppBindHost(%q) accepted; want a refusal", bad)
		} else if !strings.Contains(err.Error(), "webhook_bind") {
			t.Errorf("whatsAppBindHost(%q) err = %v, want it to name webhook_bind", bad, err)
		}
	}
}

// rysh doctor / rysh setup's check (Validate) refuses a direct config without
// app_secret too, before any Graph call, so it never reports PASS for a
// profile whose adapter would refuse to start.
func TestE25WhatsAppValidateRequiresAppSecret(t *testing.T) {
	t.Chdir(t.TempDir()) // the adapter persists its store under the cwd
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls++
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{"id":"100000000000001"}`))
	}))
	defer srv.Close()
	for _, secret := range []string{"", "${WHATSAPP_APP_SECRET}"} {
		a := NewWhatsAppAdapter(msg.ChannelConfig{Phone: "100000000000001", APIKey: "e25-synthetic-graph-token", AppSecret: secret})
		a.baseURL = srv.URL
		err := a.Validate(context.Background())
		if err == nil || !strings.Contains(err.Error(), "app_secret") {
			t.Errorf("AppSecret %q: Validate err = %v, want a refusal naming app_secret", secret, err)
		}
	}
	if calls != 0 {
		t.Errorf("Validate made %d Graph call(s) before refusing", calls)
	}
}
