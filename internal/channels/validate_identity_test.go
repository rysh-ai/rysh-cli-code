// SPDX-License-Identifier: Apache-2.0

package channels

// E33 G3: non-binding credential checks for Telegram (Bot API getMe) and
// Discord (GET /users/@me + the application id the invite link needs), with
// the bot identity exposed so setup can print "message @xyz_bot now".
//
// Every server here is a local httptest stub; no test talks to Telegram,
// Discord or Slack. Tokens are synthetic. A failure message never prints an
// error that might carry the token — it names the check instead.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	fakeTelegramToken = "424242:FAKE-e33-telegram-token"
	fakeDiscordToken  = "FAKE.e33.discord-token"
)

// recordingServer is a stub API that records every request path so a test can
// prove Validate made only the identity calls (no poll, no webhook, no gateway).
type recordingServer struct {
	mu    sync.Mutex
	paths []string
	auth  []string
}

func (r *recordingServer) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, req.URL.Path)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
}

func (r *recordingServer) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func TestTelegramValidateMissingToken(t *testing.T) {
	a := NewTelegramAdapter(msg.ChannelConfig{})
	if err := a.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "bot_token") {
		t.Errorf("err = %v, want bot_token requirement", err)
	}
}

func TestTelegramValidateIdentity(t *testing.T) {
	rec := &recordingServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.URL.Path != "/bot"+fakeTelegramToken+"/getMe" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"ok":false,"error_code":404,"description":"Not Found"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":{"id":4242,"is_bot":true,"first_name":"XYZ","username":"xyz_bot"}}`))
	}))
	defer srv.Close()

	a := NewTelegramAdapter(msg.ChannelConfig{BotToken: fakeTelegramToken})
	a.apiBase = srv.URL
	id, err := a.ValidateIdentity(context.Background())
	if err != nil {
		t.Fatalf("valid token: Validate failed (error not printed: may carry the token)")
	}
	if id.ID != "4242" || id.Username != "xyz_bot" {
		t.Errorf("identity = %+v, want id 4242 username xyz_bot", id)
	}
	if got := id.Handle(); got != "@xyz_bot" {
		t.Errorf("Handle() = %q, want @xyz_bot", got)
	}
	if err := a.Validate(context.Background()); err != nil {
		t.Errorf("Validate (error-only form) failed")
	}
	// Non-binding: getMe only — never getUpdates or setWebhook — and the
	// adapter is not marked connected by a credential check.
	for _, p := range rec.seen() {
		if !strings.HasSuffix(p, "/getMe") {
			t.Errorf("Validate called a non-identity method (path suffix %q)", p[strings.LastIndex(p, "/"):])
		}
	}
	if a.Status().Connected {
		t.Error("Validate marked the adapter connected")
	}
}

func TestTelegramValidateRejectedToken(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"401", http.StatusUnauthorized, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, "rejected"},
		// The Bot API answers 404 for a token whose shape it cannot route.
		{"404", http.StatusNotFound, `{"ok":false,"error_code":404,"description":"Not Found"}`, "not recognised"},
		{"500 non-json", http.StatusInternalServerError, `<html>oops</html>`, "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			a := NewTelegramAdapter(msg.ChannelConfig{BotToken: fakeTelegramToken})
			a.apiBase = srv.URL
			_, err := a.ValidateIdentity(context.Background())
			if err == nil {
				t.Fatal("rejected token validated")
			}
			if strings.Contains(err.Error(), fakeTelegramToken) {
				t.Fatal("error text carries the bot token")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "telegram") {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestTelegramTransportErrorRedactsToken: the Bot API puts the token in the URL
// path, and net/http's *url.Error quotes the URL. Any transport failure (here a
// closed server) must surface without it — this error reaches doctor output,
// the channel status line and the logs.
func TestTelegramTransportErrorRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close()

	a := NewTelegramAdapter(msg.ChannelConfig{BotToken: fakeTelegramToken})
	a.apiBase = base
	_, err := a.ValidateIdentity(context.Background())
	if err == nil {
		t.Fatal("closed server validated")
	}
	if strings.Contains(err.Error(), fakeTelegramToken) {
		t.Fatal("transport error carries the bot token")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("err = %v, want an unreachable decode", err)
	}

	// The same redaction covers the runtime path (Start → getMe), whose error
	// lands in Status().Error.
	if serr := a.Start(context.Background()); serr == nil {
		_ = a.Stop()
		t.Fatal("Start against a closed server succeeded")
	} else if strings.Contains(serr.Error(), fakeTelegramToken) {
		t.Fatal("Start error carries the bot token")
	}
	if strings.Contains(a.Status().Error, fakeTelegramToken) {
		t.Fatal("Status().Error carries the bot token")
	}
}

// TestValidateUnresolvedRefMakesNoRequest: doctor resolves ${NAME} refs and
// leaves an unset one verbatim. Validate must refuse that locally — no request
// to the stub (and so, in production, none to the platform).
func TestValidateUnresolvedRefMakesNoRequest(t *testing.T) {
	rec := &recordingServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	const ref = "${E33_FAKE_UNSET_TOKEN}"

	tg := NewTelegramAdapter(msg.ChannelConfig{BotToken: ref})
	tg.apiBase = srv.URL
	if err := tg.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "bot_token") {
		t.Errorf("telegram err = %v, want bot_token requirement", err)
	}
	dc := NewDiscordAdapter(msg.ChannelConfig{BotToken: ref})
	dc.apiBase = srv.URL
	if err := dc.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "bot_token") {
		t.Errorf("discord err = %v, want bot_token requirement", err)
	}
	old := slackValidateAPIURL
	slackValidateAPIURL = srv.URL + "/"
	defer func() { slackValidateAPIURL = old }()
	sl := NewSlackAdapter(msg.ChannelConfig{BotToken: "${E33_FAKE_UNSET_SLACK}", AppToken: "xapp-fake"})
	if err := sl.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "bot_token") {
		t.Errorf("slack err = %v, want bot_token requirement", err)
	}
	if n := len(rec.seen()); n != 0 {
		t.Errorf("unresolved reference reached the API: %d request(s)", n)
	}
}

func TestDiscordValidateMissingToken(t *testing.T) {
	a := NewDiscordAdapter(msg.ChannelConfig{})
	if err := a.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "bot_token") {
		t.Errorf("err = %v, want bot_token requirement", err)
	}
}

// fakeDiscordAPI answers /users/@me and /oauth2/applications/@me for the good
// token and 401 for anything else.
func fakeDiscordAPI(rec *recordingServer, userBody, appBody string, appStatus int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bot "+fakeDiscordToken {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"401: Unauthorized","code":0}`))
			return
		}
		switch r.URL.Path {
		case "/users/@me":
			w.Write([]byte(userBody))
		case "/oauth2/applications/@me":
			w.WriteHeader(appStatus)
			w.Write([]byte(appBody))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"404: Not Found","code":0}`))
		}
	}
}

func TestDiscordValidateIdentity(t *testing.T) {
	rec := &recordingServer{}
	srv := httptest.NewServer(fakeDiscordAPI(rec,
		`{"id":"111222333444555666","username":"xyz","bot":true}`,
		`{"id":"999888777666555444","name":"xyz app"}`, http.StatusOK))
	defer srv.Close()

	a := NewDiscordAdapter(msg.ChannelConfig{BotToken: fakeDiscordToken})
	a.apiBase = srv.URL
	id, err := a.ValidateIdentity(context.Background())
	if err != nil {
		t.Fatalf("valid token: Validate failed (error not printed: may carry the token)")
	}
	if id.ID != "111222333444555666" || id.Username != "xyz" || id.ApplicationID != "999888777666555444" {
		t.Errorf("identity = %+v", id)
	}
	if id.Handle() != "@xyz" {
		t.Errorf("Handle() = %q, want @xyz", id.Handle())
	}
	// Non-binding: exactly the two REST identity GETs, no gateway.
	got := rec.seen()
	if len(got) != 2 || got[0] != "/users/@me" || got[1] != "/oauth2/applications/@me" {
		t.Errorf("requests = %v, want [/users/@me /oauth2/applications/@me]", got)
	}
	if a.Status().Connected {
		t.Error("Validate marked the adapter connected")
	}
}

func TestDiscordValidateRejected(t *testing.T) {
	rec := &recordingServer{}
	srv := httptest.NewServer(fakeDiscordAPI(rec, `{}`, `{}`, http.StatusOK))
	defer srv.Close()

	a := NewDiscordAdapter(msg.ChannelConfig{BotToken: "FAKE.wrong.token"})
	a.apiBase = srv.URL
	_, err := a.ValidateIdentity(context.Background())
	if err == nil {
		t.Fatal("wrong token validated")
	}
	if strings.Contains(err.Error(), "FAKE.wrong.token") {
		t.Fatal("error text carries the bot token")
	}
	if !strings.Contains(err.Error(), "discord") || !strings.Contains(err.Error(), "rejected") || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want a decoded 401", err)
	}
}

func TestDiscordValidate404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"404: Not Found","code":0}`))
	}))
	defer srv.Close()
	a := NewDiscordAdapter(msg.ChannelConfig{BotToken: fakeDiscordToken})
	a.apiBase = srv.URL
	_, err := a.ValidateIdentity(context.Background())
	if err == nil {
		t.Fatal("404 validated")
	}
	if strings.Contains(err.Error(), fakeDiscordToken) {
		t.Fatal("error text carries the bot token")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want a 404 decode", err)
	}
}

// TestDiscordValidateUserTokenRefused: a pasted user (non-bot) token
// authenticates /users/@me but cannot run a bot — say so at setup.
func TestDiscordValidateUserTokenRefused(t *testing.T) {
	rec := &recordingServer{}
	srv := httptest.NewServer(fakeDiscordAPI(rec,
		`{"id":"5","username":"human","bot":false}`, `{"id":"6"}`, http.StatusOK))
	defer srv.Close()
	a := NewDiscordAdapter(msg.ChannelConfig{BotToken: fakeDiscordToken})
	a.apiBase = srv.URL
	if _, err := a.ValidateIdentity(context.Background()); err == nil || !strings.Contains(err.Error(), "not a bot") {
		t.Errorf("err = %v, want a not-a-bot refusal", err)
	}
}

// TestDiscordValidateNoApplicationID: the token is valid even when the
// application lookup fails; the identity then carries no application id (the
// invite-link step falls back to the portal) rather than failing validation.
func TestDiscordValidateNoApplicationID(t *testing.T) {
	rec := &recordingServer{}
	srv := httptest.NewServer(fakeDiscordAPI(rec,
		`{"id":"7","username":"xyz","bot":true}`, `{"message":"nope"}`, http.StatusForbidden))
	defer srv.Close()
	a := NewDiscordAdapter(msg.ChannelConfig{BotToken: fakeDiscordToken})
	a.apiBase = srv.URL
	id, err := a.ValidateIdentity(context.Background())
	if err != nil {
		t.Fatalf("token valid but application lookup failed: Validate should pass")
	}
	if id.ID != "7" || id.ApplicationID != "" {
		t.Errorf("identity = %+v, want id 7 and no application id", id)
	}
}

func TestDiscordTransportErrorNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close()
	a := NewDiscordAdapter(msg.ChannelConfig{BotToken: fakeDiscordToken})
	a.apiBase = base
	_, err := a.ValidateIdentity(context.Background())
	if err == nil {
		t.Fatal("closed server validated")
	}
	if strings.Contains(err.Error(), fakeDiscordToken) {
		t.Fatal("transport error carries the bot token")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("err = %v, want unreachable", err)
	}
}

// TestSlackValidateIdentity: Slack's auth.test already ran in Validate; the
// identity form exposes the bot user so setup can name it too.
func TestSlackValidateIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"user_id":"U0BOT","user":"rysh_bot","team":"T","team_id":"T1"}`))
	}))
	defer srv.Close()
	old := slackValidateAPIURL
	slackValidateAPIURL = srv.URL + "/"
	defer func() { slackValidateAPIURL = old }()

	a := NewSlackAdapter(msg.ChannelConfig{BotToken: "xoxb-fake", AppToken: "xapp-fake"})
	id, err := a.ValidateIdentity(context.Background())
	if err != nil {
		t.Fatalf("ValidateIdentity: %v", err)
	}
	if id.ID != "U0BOT" || id.Username != "rysh_bot" || id.Handle() != "@rysh_bot" {
		t.Errorf("identity = %+v", id)
	}
}

func TestIdentityValidatorCoverage(t *testing.T) {
	var _ IdentityValidator = (*TelegramAdapter)(nil)
	var _ IdentityValidator = (*DiscordAdapter)(nil)
	var _ IdentityValidator = (*SlackAdapter)(nil)
	var _ Validator = (*TelegramAdapter)(nil)
	var _ Validator = (*DiscordAdapter)(nil)
	if (BotIdentity{}).Handle() != "" {
		t.Error("empty identity should have an empty handle")
	}
}
