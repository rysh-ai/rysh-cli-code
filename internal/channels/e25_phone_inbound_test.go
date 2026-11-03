// SPDX-License-Identifier: Apache-2.0

package channels

// E25 (channels owner): the Twilio phone adapter must authenticate every
// inbound POST.
//
// Before this fix, signature checks ran only when webhook_url was set. Without
// it the adapter started, logged a warning, and accepted any POST that reached
// its port as a message from whatever "From" number the body claimed. Now:
//   - Start refuses without auth_token or webhook_url (empty, blank or an
//     unresolved ${…} reference counts as missing), and webhook_url must be an
//     absolute http(s) URL;
//   - Validate (rysh doctor / setup's check) applies the same gate, so such a
//     profile never reads PASS, and makes no network call;
//   - the handler refuses every unsigned POST, whatever the config.
//
// Every test runs in a temp cwd: an accepted message is persisted under
// .rysh/channel-state/ relative to the working directory.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func e25PhoneFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return strconv.Itoa(port)
}

func e25PhoneBase() msg.ChannelConfig {
	return msg.ChannelConfig{
		AccountSID: "ACtest",
		AuthToken:  "tok-test",
		Number:     "+15550100",
		Provider:   "twilio",
		WebhookURL: testPublicURL,
	}
}

// Start must refuse a config that cannot authenticate inbound, and name the key.
func TestE25PhoneStartRefusesUnverifiableInbound(t *testing.T) {
	t.Chdir(t.TempDir())
	cases := []struct {
		name string
		mod  func(*msg.ChannelConfig)
		want string
	}{
		{"no webhook_url", func(c *msg.ChannelConfig) { c.WebhookURL = "" }, "webhook_url"},
		{"blank webhook_url", func(c *msg.ChannelConfig) { c.WebhookURL = "   " }, "webhook_url"},
		{"unresolved webhook_url", func(c *msg.ChannelConfig) { c.WebhookURL = "${TWILIO_WEBHOOK_URL}" }, "webhook_url"},
		{"relative webhook_url", func(c *msg.ChannelConfig) { c.WebhookURL = "tunnel.example/sms" }, "webhook_url"},
		{"non-http webhook_url", func(c *msg.ChannelConfig) { c.WebhookURL = "ftp://tunnel.example/sms" }, "webhook_url"},
		{"blank auth_token", func(c *msg.ChannelConfig) { c.AuthToken = "  " }, "auth_token"},
		{"unresolved auth_token", func(c *msg.ChannelConfig) { c.AuthToken = "${TWILIO_AUTH_TOKEN}" }, "auth_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := e25PhoneBase()
			cfg.WebhookPort = e25PhoneFreePort(t)
			tc.mod(&cfg)
			p := NewPhoneAdapter(cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := p.Start(ctx)
			if err == nil {
				_ = p.Stop()
				t.Fatalf("Start must refuse (%s): inbound could not be authenticated", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %q, got %v", tc.want, err)
			}
			if p.Status().Connected {
				t.Error("Status must not report Connected after a refused Start")
			}
		})
	}
}

// A complete config still starts (control).
func TestE25PhoneStartAcceptsCompleteConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := e25PhoneBase()
	cfg.WebhookPort = e25PhoneFreePort(t)
	p := NewPhoneAdapter(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start with auth_token and webhook_url: %v", err)
	}
	_ = p.Stop()
}

// Validate must apply the same gate, locally, before any request.
func TestE25PhoneValidateRefusesWithoutWebhookURL(t *testing.T) {
	t.Chdir(t.TempDir())
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = rw.Write([]byte(`{"sid":"ACtest","status":"active"}`))
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		mod  func(*msg.ChannelConfig)
		want string
	}{
		{"no webhook_url", func(c *msg.ChannelConfig) { c.WebhookURL = "" }, "webhook_url"},
		{"unresolved auth_token", func(c *msg.ChannelConfig) { c.AuthToken = "${TWILIO_AUTH_TOKEN}" }, "auth_token"},
	} {
		cfg := e25PhoneBase()
		tc.mod(&cfg)
		p := NewPhoneAdapter(cfg)
		p.baseURL = srv.URL
		err := p.Validate(context.Background())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("Validate made %d request(s) for a config it must refuse locally", n)
	}
}

// The handler refuses every unsigned POST, even on an adapter whose config
// never declared webhook_url (built directly, bypassing Start).
func TestE25PhoneHandlerRefusesUnsignedWithoutWebhookURL(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := e25PhoneBase()
	cfg.WebhookURL = ""
	p := NewPhoneAdapter(cfg)

	form := inboundForm("SM-e25-1", "+15550142", "approve the deploy")
	rec := postForm(p, form, "")
	if rec.Code < 400 {
		t.Errorf("unsigned POST without webhook_url: status %d, want a refusal (4xx)", rec.Code)
	}
	// A signature over some URL must not help either: with no declared URL
	// there is nothing to verify it against.
	rec = postForm(p, form, signTwilio(testPublicURL, form, cfg.AuthToken))
	if rec.Code < 400 {
		t.Errorf("signed POST without webhook_url: status %d, want a refusal (4xx)", rec.Code)
	}
	if got := drainPhone(p); len(got) != 0 {
		t.Fatalf("%d unauthenticated message(s) reached the humanoid", len(got))
	}
}

// With webhook_url set, an unsigned POST is refused and a signed one accepted.
func TestE25PhoneHandlerSignedOnly(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := e25PhoneBase()
	p := NewPhoneAdapter(cfg)

	form := inboundForm("SM-e25-2", "+15550142", "hello")
	if rec := postForm(p, form, ""); rec.Code < 400 {
		t.Errorf("unsigned POST: status %d, want a refusal (4xx)", rec.Code)
	}
	if got := drainPhone(p); len(got) != 0 {
		t.Fatalf("an unsigned message reached the humanoid")
	}
	if rec := postForm(p, form, signTwilio(testPublicURL, form, cfg.AuthToken)); rec.Code != http.StatusOK {
		t.Fatalf("signed POST: status %d, want 200", rec.Code)
	}
	if got := drainPhone(p); len(got) != 1 {
		t.Fatalf("signed POST delivered %d message(s), want 1", len(got))
	}
}

// e20 N1: the handler must apply checkConfig's rule. An unresolved ${…}
// auth_token is not a key: a body "signed" with that literal text must be
// refused, even on an adapter that was never started.
func TestE25PhoneHandlerRefusesUnresolvedAuthToken(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := e25PhoneBase()
	cfg.AuthToken = "${TWILIO_AUTH_TOKEN}"
	p := NewPhoneAdapter(cfg)

	form := inboundForm("SM-e25-n1", "+15550142", "approve")
	if rec := postForm(p, form, signTwilio(cfg.WebhookURL, form, cfg.AuthToken)); rec.Code < 400 {
		t.Errorf("POST keyed with an unresolved auth_token: status %d, want a refusal (4xx)", rec.Code)
	}
	if got := drainPhone(p); len(got) != 0 {
		t.Fatal("a message keyed with an unresolved auth_token reached the humanoid")
	}
}

// e20 N3: plain http is allowed (Twilio supports it) but flagged when the host
// is not loopback, because the body and signature then cross the network in
// the clear.
func TestE25PhoneWebhookURLCleartextRemote(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://tunnel.example/sms", false},
		{"http://tunnel.example/sms", true},
		{"http://203.0.113.7:8080/sms", true},
		{"http://127.0.0.1:23312/sms", false},
		{"http://localhost/sms", false},
		{"http://[::1]/sms", false},
	} {
		if got := twilioWebhookURLCleartextRemote(tc.url); got != tc.want {
			t.Errorf("twilioWebhookURLCleartextRemote(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
