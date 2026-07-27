package channels

// Tests for the optional non-binding Validator interface (design 004 §4.3):
// Slack auth.test and the WhatsApp Graph GET, exercised against local stub
// servers — no port/webhook is ever bound by Validate itself.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestSlackValidateMissingTokens: Validate fails fast without network when
// tokens are absent.
func TestSlackValidateMissingTokens(t *testing.T) {
	a := NewSlackAdapter(msg.ChannelConfig{})
	if err := a.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "bot_token") {
		t.Errorf("err = %v, want bot_token requirement", err)
	}
	a = NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb-x"})
	if err := a.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "app_token") {
		t.Errorf("err = %v, want app_token requirement", err)
	}
}

// TestSlackValidateAuthTest drives the real auth.test HTTP path against a
// stub Slack API (slackValidateAPIURL override).
func TestSlackValidateAuthTest(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "auth.test") {
			http.NotFound(w, r)
			return
		}
		// slack-go sends the token as a form field or an Authorization header
		// depending on version; accept either.
		_ = r.ParseForm()
		gotAuth = r.Header.Get("Authorization") + " " + r.FormValue("token")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(gotAuth, "xoxb-good") {
			w.Write([]byte(`{"ok":true,"user_id":"U1","team":"T"}`))
			return
		}
		w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	defer srv.Close()
	old := slackValidateAPIURL
	slackValidateAPIURL = srv.URL + "/"
	defer func() { slackValidateAPIURL = old }()

	good := NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb-good", AppToken: "xapp-1"})
	if err := good.Validate(context.Background()); err != nil {
		t.Errorf("valid creds: %v", err)
	}
	if !strings.Contains(gotAuth, "xoxb-good") {
		t.Errorf("bot token not sent: %q", gotAuth)
	}

	bad := NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb-bad", AppToken: "xapp-1"})
	if err := bad.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("err = %v, want invalid_auth decode", err)
	}

	// A bot token that authenticates but a malformed app token is caught too.
	weird := NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb-good", AppToken: "not-an-app-token"})
	if err := weird.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "xapp") {
		t.Errorf("err = %v, want app-token shape complaint", err)
	}
}

// TestWhatsAppValidate drives the Graph GET against a stub server via the
// adapter's overridable baseURL.
func TestWhatsAppValidate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case auth == "Bearer good-token":
			w.Write([]byte(`{"id":"12345","display_phone_number":"+1 555"}`))
		case strings.Contains(r.URL.Path, "unknown-phone"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"Unknown path components"}}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"Invalid OAuth access token"}}`))
		}
	}))
	defer srv.Close()

	newAdapter := func(phone, key string) *WhatsAppAdapter {
		a := NewWhatsAppAdapter(msg.ChannelConfig{Phone: phone, APIKey: key})
		a.baseURL = srv.URL
		return a
	}

	if err := newAdapter("12345", "good-token").Validate(context.Background()); err != nil {
		t.Errorf("valid creds: %v", err)
	}
	if err := newAdapter("12345", "expired").Validate(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "token rejected") || !strings.Contains(err.Error(), "Invalid OAuth access token") {
		t.Errorf("err = %v, want decoded 401", err)
	}
	if err := newAdapter("", "good-token").Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "phone") {
		t.Errorf("err = %v, want phone requirement", err)
	}
	if err := newAdapter("12345", "").Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Errorf("err = %v, want api_key requirement", err)
	}

	// Context timeout decodes as unreachable, not a hang.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer slow.Close()
	a := NewWhatsAppAdapter(msg.ChannelConfig{Phone: "12345", APIKey: "good-token"})
	a.baseURL = slow.URL
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.Validate(ctx); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("err = %v, want unreachable decode", err)
	}
}

// TestValidatorInterfaceCoverage documents which adapters offer the
// non-binding check (doctor WARNs on the rest instead of Start-probing).
func TestValidatorInterfaceCoverage(t *testing.T) {
	var _ Validator = (*SlackAdapter)(nil)
	var _ Validator = (*WhatsAppAdapter)(nil)

	// PhoneAdapter implements Validator deliberately (X3): it is a placeholder
	// that sends and receives nothing, so `rysh doctor` should report a hard
	// FAIL with the reason rather than the "not validated" WARN that adapters
	// without a non-binding check receive. An unimplemented channel is a
	// known-bad credential set, not an unknown one.
	v, ok := any(NewPhoneAdapter(msg.ChannelConfig{})).(Validator)
	if !ok {
		t.Fatal("PhoneAdapter must implement Validator so doctor FAILs rather than WARNs")
	}
	if err := v.Validate(context.Background()); err == nil {
		t.Error("PhoneAdapter.Validate must fail — the adapter is not implemented")
	}

	// Adapters that genuinely have no non-binding check must NOT accidentally
	// satisfy the interface — doctor relies on the assertion to pick the WARN
	// path instead of Start-probing (which binds ports/webhooks).
	if _, ok := any(NewChatbotAdapter(msg.ChannelConfig{})).(Validator); ok {
		t.Error("ChatbotAdapter unexpectedly implements Validator; update doctor expectations")
	}
}
