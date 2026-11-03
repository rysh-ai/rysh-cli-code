// SPDX-License-Identifier: Apache-2.0

package channels

// WeChat and WeCom bind their webhook listener to 127.0.0.1 unless
// webhook_bind names another host, the same rule as WhatsApp, LINE, Teams and
// Telegram: the operator's reverse proxy or tunnel forwards to loopback.
// Before this they bound every interface (":port"), so anyone on the LAN
// could reach the webhook directly. Both still verify every POST; this is
// defence in depth.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
)

func bindTestTokenOK(rw http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"errcode": 0, "errmsg": "ok", "access_token": "tok-bind", "expires_in": 7200,
	})
}

type bindTestAdapter interface {
	Start(context.Context) error
	Stop() error
	listenAddr() string
}

func newBindTestWeChat(t *testing.T, bind string) bindTestAdapter {
	w := newWeChatTestAdapter(t, bindTestTokenOK)
	w.config.VerifyToken = "wx-token"
	w.config.WebhookBind = bind
	w.port = 0
	return w
}

func newBindTestWeCom(t *testing.T, bind string) bindTestAdapter {
	w := newWeComTestAdapter(t, bindTestTokenOK)
	w.config.WebhookBind = bind
	w.port = 0
	return w
}

var bindTestAdapters = []struct {
	name string
	make func(*testing.T, string) bindTestAdapter
}{
	{"wechat", newBindTestWeChat},
	{"wecom", newBindTestWeCom},
}

func boundHost(t *testing.T, a bindTestAdapter) string {
	t.Helper()
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	host, _, err := net.SplitHostPort(a.listenAddr())
	if err != nil {
		t.Fatalf("listen address %q: %v", a.listenAddr(), err)
	}
	return host
}

func TestWebhookBindLoopbackByDefaultWeChatWeCom(t *testing.T) {
	for _, tc := range bindTestAdapters {
		t.Run(tc.name, func(t *testing.T) {
			if host := boundHost(t, tc.make(t, "")); host != "127.0.0.1" {
				t.Fatalf("with no webhook_bind the webhook bound %q, want 127.0.0.1", host)
			}
		})
	}
}

func TestWebhookBindExplicitHostWeChatWeCom(t *testing.T) {
	for _, tc := range bindTestAdapters {
		t.Run(tc.name, func(t *testing.T) {
			// Go reports a 0.0.0.0 listener as "::" (dual-stack wildcard), so
			// check for an unspecified address, not the literal text. This is a
			// no-regression check for the opt-out; the old ":port" bind passed
			// it too.
			host := boundHost(t, tc.make(t, "0.0.0.0"))
			if ip := net.ParseIP(host); ip == nil || !ip.IsUnspecified() {
				t.Fatalf("with webhook_bind 0.0.0.0 the webhook bound %q, want every interface", host)
			}
		})
	}
}

func TestWebhookBindRefusesHostnameWeChatWeCom(t *testing.T) {
	for _, tc := range bindTestAdapters {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.make(t, "example.com")
			err := a.Start(context.Background())
			if err == nil {
				_ = a.Stop()
				t.Fatal("a webhook_bind that is not an IP address or localhost must be refused")
			}
			if !strings.Contains(err.Error(), "webhook_bind") {
				t.Fatalf("error %q should name webhook_bind", err)
			}
		})
	}
}
