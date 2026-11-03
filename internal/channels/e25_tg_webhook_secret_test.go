// SPDX-License-Identifier: Apache-2.0

package channels

// E25: Telegram webhook mode must verify inbound updates.
//
// Before this fix, setWebhook was sent without a secret_token and the handler
// checked nothing, so anyone who could reach the tunnel URL could post an
// update with any from.id (including the owner's). The fix: the adapter mints
// a random secret_token on every Start, sends it with setWebhook, and the
// handler answers 401 unless X-Telegram-Bot-Api-Secret-Token matches it.
//
// All Bot API traffic is faked with httptest; the webhook listener is loopback.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Telegram allows 1-256 chars of A-Z, a-z, 0-9, _ and -. We also require at
// least 32 chars so the token carries real entropy.
var e25TGSecretShape = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)

const e25TGSecretHeader = "X-Telegram-Bot-Api-Secret-Token"

// e25TGFakeAPI is a fake Bot API that records each setWebhook secret_token.
type e25TGFakeAPI struct {
	srv     *httptest.Server
	mu      sync.Mutex
	secrets []string
}

func newE25TGFakeAPI(t *testing.T) *e25TGFakeAPI {
	t.Helper()
	f := &e25TGFakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = io.WriteString(rw, `{"ok":true,"result":{"id":424242,"is_bot":true,"first_name":"rysh","username":"ryshbot"}}`)
		case strings.HasSuffix(r.URL.Path, "/setWebhook"):
			var p struct {
				SecretToken string `json:"secret_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			f.mu.Lock()
			f.secrets = append(f.secrets, p.SecretToken)
			f.mu.Unlock()
			_, _ = io.WriteString(rw, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/deleteWebhook"):
			_, _ = io.WriteString(rw, `{"ok":true,"result":true}`)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *e25TGFakeAPI) registered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.secrets...)
}

func e25TGFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// e25TGStartWebhook starts a webhook-mode adapter against the fake API and
// returns it with its loopback port. The adapter is stopped on cleanup.
func e25TGStartWebhook(t *testing.T, api *e25TGFakeAPI) (*TelegramAdapter, int) {
	t.Helper()
	port := e25TGFreePort(t)
	a := NewTelegramAdapter(msg.ChannelConfig{
		BotToken:    "TOKEN",
		Mode:        "webhook",
		WebhookPort: strconv.Itoa(port),
		WebhookURL:  "https://example.com/tg-hook",
	})
	a.apiBase = api.srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	if err := a.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start (webhook): %v", err)
	}
	t.Cleanup(func() {
		_ = a.Stop()
		cancel()
	})
	return a, port
}

func e25TGPost(t *testing.T, port int, secret string, set bool) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/", port),
		strings.NewReader(tgFixtureDM))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if set {
		req.Header.Set(e25TGSecretHeader, secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST to webhook: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func e25TGNoInbound(t *testing.T, a *TelegramAdapter, why string) {
	t.Helper()
	select {
	case im := <-a.InboundCh():
		t.Fatalf("%s: a forged update reached inbound (content %q)", why, im.Content)
	case <-time.After(200 * time.Millisecond):
	}
}

// setWebhook must carry a fresh, well-formed secret_token on every Start.
func TestE25TelegramWebhookRegistersSecret(t *testing.T) {
	api := newE25TGFakeAPI(t)
	a1, _ := e25TGStartWebhook(t, api)
	a2, _ := e25TGStartWebhook(t, api)

	got := api.registered()
	if len(got) != 2 {
		t.Fatalf("setWebhook calls = %d, want 2", len(got))
	}
	for i, s := range got {
		if !e25TGSecretShape.MatchString(s) {
			t.Errorf("start %d: setWebhook secret_token is missing or malformed "+
				"(want 32-256 chars of [A-Za-z0-9_-])", i+1)
		}
	}
	if got[0] != "" && got[0] == got[1] {
		t.Error("two Starts registered the same secret_token; it must be minted per start")
	}
	// The secret must not leak into Status.
	for _, a := range []*TelegramAdapter{a1, a2} {
		st := a.Status()
		for _, s := range got {
			if s != "" && (strings.Contains(st.Details, s) || strings.Contains(st.Error, s)) {
				t.Error("Status exposes the webhook secret_token")
			}
		}
	}
}

// Forged updates (no header, wrong header) get 401 and never reach inbound;
// the registered secret is accepted.
func TestE25TelegramWebhookRefusesForged(t *testing.T) {
	api := newE25TGFakeAPI(t)
	a, port := e25TGStartWebhook(t, api)

	if code := e25TGPost(t, port, "", false); code != http.StatusUnauthorized {
		t.Errorf("POST without secret header: status %d, want 401", code)
	}
	e25TGNoInbound(t, a, "no header")

	if code := e25TGPost(t, port, "forged", true); code != http.StatusUnauthorized {
		t.Errorf("POST with a wrong secret header: status %d, want 401", code)
	}
	e25TGNoInbound(t, a, "wrong header")

	if code := e25TGPost(t, port, "", true); code != http.StatusUnauthorized {
		t.Errorf("POST with an empty secret header: status %d, want 401", code)
	}
	e25TGNoInbound(t, a, "empty header")

	reg := api.registered()
	if len(reg) != 1 || reg[0] == "" {
		t.Fatal("no secret_token was registered with setWebhook, so no genuine update can be sent")
	}
	if code := e25TGPost(t, port, reg[0], true); code != http.StatusOK {
		t.Fatalf("POST with the registered secret: status %d, want 200", code)
	}
	select {
	case im := <-a.InboundCh():
		if im.Content != "hello bot" {
			t.Errorf("inbound content = %q", im.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the genuine webhook update")
	}
}

// A handler with no minted secret (never started in webhook mode) must refuse
// everything, including an empty header: an empty secret never matches.
func TestE25TelegramHandlerFailsClosedWithoutSecret(t *testing.T) {
	a := NewTelegramAdapter(msg.ChannelConfig{BotToken: "TOKEN", Mode: "webhook"})
	for _, tc := range []struct {
		name   string
		header string
		set    bool
	}{
		{"no header", "", false},
		{"empty header", "", true},
		{"some header", "anything", true},
	} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tgFixtureDM))
		if tc.set {
			req.Header.Set(e25TGSecretHeader, tc.header)
		}
		rec := httptest.NewRecorder()
		a.handleWebhook(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", tc.name, rec.Code)
		}
	}
	e25TGNoInbound(t, a, "unstarted handler")
}

// After Stop, the secret of that start must be forgotten: a late request
// carrying it is refused (e20 hygiene note on fa51e02).
func TestE25TelegramStopForgetsSecret(t *testing.T) {
	api := newE25TGFakeAPI(t)
	a, _ := e25TGStartWebhook(t, api)
	reg := api.registered()
	if len(reg) != 1 || reg[0] == "" {
		t.Fatal("no secret_token was registered with setWebhook")
	}
	_ = a.Stop()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tgFixtureDM))
	req.Header.Set(e25TGSecretHeader, reg[0])
	rec := httptest.NewRecorder()
	a.handleWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST with the old secret after Stop: status %d, want 401", rec.Code)
	}
	e25TGNoInbound(t, a, "after Stop")
}
