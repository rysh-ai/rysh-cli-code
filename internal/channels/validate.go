// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/slack-go/slack"
)

// Validator is implemented by adapters that can check their credentials
// WITHOUT establishing a full connection or binding any port/webhook (e.g.
// Slack auth.test, a WhatsApp Graph token GET). `rysh doctor` prefers this
// non-binding check; adapters that don't implement it are reported as
// "not validated" rather than probed via Start(), which has side effects
// (socket-mode connections, webhook listeners). See design 004 §4.3.
type Validator interface {
	Validate(ctx context.Context) error
}

// BotIdentity is what a non-binding credential check learned about the bot
// account behind a token. Setup prints it ("message @xyz_bot now") so the user
// knows exactly which account to talk to, and the Discord invite link is built
// from ApplicationID.
type BotIdentity struct {
	ID       string // the bot's own user id on the platform
	Username string // without the leading "@"
	// ApplicationID is the Discord application (client) id the OAuth2 invite
	// URL needs. Empty on other channels, and on Discord when the application
	// lookup failed — the token itself was still valid.
	ApplicationID string
}

// Handle returns "@username", or "" when the username is unknown.
func (b BotIdentity) Handle() string {
	if b.Username == "" {
		return ""
	}
	return "@" + b.Username
}

// IdentityValidator is a Validator that also reports who the bot is. Like
// Validate it is non-binding: one or two REST identity calls, never a poll
// loop, webhook or gateway connection.
type IdentityValidator interface {
	Validator
	ValidateIdentity(ctx context.Context) (BotIdentity, error)
}

// unresolvedTokenRef refuses a bot_token that is still a "${NAME}" reference.
// Callers resolve references before validating, and an unset one is left in
// place verbatim; sending that literal to the platform would only earn a 401
// for a reason the user cannot see — and is a network call that tells us
// nothing. The message is all bot_token and no value, because the reference
// is not the token but sits where one should be.
func unresolvedTokenRef(channel, token string) error {
	if strings.Contains(token, "${") {
		return fmt.Errorf("%s: bot_token is required (its ${…} reference is unset — "+
			"add the token with ##secret new or export it)", channel)
	}
	return nil
}

// slackValidateAPIURL overrides the Slack API base URL used by
// SlackAdapter.Validate. Empty means the real API; tests point it at a local
// httptest server. It must end with "/" (slack-go convention).
var slackValidateAPIURL string

// Validate checks the Slack credentials with a single auth.test call — the
// same call Start() makes first — without opening the Socket Mode connection.
// It verifies the bot token remotely; the app-level token is only checked for
// presence/shape (Slack offers no cheap non-binding check for app tokens —
// they are exercised when the socket connects).
func (s *SlackAdapter) Validate(ctx context.Context) error {
	_, err := s.ValidateIdentity(ctx)
	return err
}

// ValidateIdentity is Validate plus the bot user auth.test reported.
func (s *SlackAdapter) ValidateIdentity(ctx context.Context) (BotIdentity, error) {
	if s.config.BotToken == "" {
		return BotIdentity{}, fmt.Errorf("slack: bot_token is required")
	}
	if s.config.AppToken == "" {
		return BotIdentity{}, fmt.Errorf("slack: app_token is required")
	}
	if err := unresolvedTokenRef("slack", s.config.BotToken); err != nil {
		return BotIdentity{}, err
	}
	opts, err := slackAPIOptions(s.config.APIBase)
	if err != nil {
		return BotIdentity{}, err
	}
	if len(opts) == 0 && slackValidateAPIURL != "" {
		opts = append(opts, slack.OptionAPIURL(slackValidateAPIURL))
	}
	api := slack.New(s.config.BotToken, opts...)
	resp, err := api.AuthTestContext(ctx)
	if err != nil {
		return BotIdentity{}, fmt.Errorf("slack: auth.test failed: %w", err)
	}
	if !strings.HasPrefix(s.config.AppToken, "xapp-") {
		return BotIdentity{}, fmt.Errorf("slack: app_token does not look like an app-level token (expected xapp-…)")
	}
	return BotIdentity{ID: resp.UserID, Username: resp.User}, nil
}

// Validate checks the Telegram bot token with one getMe call — the same call
// Start makes first — without starting the getUpdates poll or registering a
// webhook.
func (t *TelegramAdapter) Validate(ctx context.Context) error {
	_, err := t.ValidateIdentity(ctx)
	return err
}

// ValidateIdentity is Validate plus the bot's id and @username, so setup can
// tell the user exactly which bot to message.
func (t *TelegramAdapter) ValidateIdentity(ctx context.Context) (BotIdentity, error) {
	if t.config.BotToken == "" {
		return BotIdentity{}, fmt.Errorf("telegram: bot_token is required")
	}
	if err := unresolvedTokenRef("telegram", t.config.BotToken); err != nil {
		return BotIdentity{}, err
	}
	resp, err := t.callAPI(ctx, t.apiClient, "getMe", struct{}{})
	if err != nil {
		var te *telegramTransportError
		if errors.As(err, &te) {
			return BotIdentity{}, fmt.Errorf("telegram: Bot API unreachable: %v", err)
		}
		return BotIdentity{}, fmt.Errorf("telegram: getMe failed: %v", err)
	}
	if !resp.OK {
		switch resp.ErrorCode {
		case http.StatusUnauthorized:
			return BotIdentity{}, fmt.Errorf("telegram: bot token rejected (HTTP 401 %s) — "+
				"copy it again from @BotFather (/mybots → your bot → API Token)", resp.Description)
		case http.StatusNotFound:
			return BotIdentity{}, fmt.Errorf("telegram: bot token not recognised (HTTP 404 %s) — "+
				"a token looks like 123456789:AA… and comes from @BotFather", resp.Description)
		default:
			return BotIdentity{}, fmt.Errorf("telegram: getMe failed (HTTP %d): %s", resp.ErrorCode, resp.Description)
		}
	}
	var me telegramUser
	if err := json.Unmarshal(resp.Result, &me); err != nil {
		return BotIdentity{}, fmt.Errorf("telegram: decode getMe result: %w", err)
	}
	return BotIdentity{ID: strconv.FormatInt(me.ID, 10), Username: me.Username}, nil
}

// Validate checks the Discord bot token with GET /users/@me — a REST call, no
// Gateway connection — and looks up the application id the invite link needs.
func (d *DiscordAdapter) Validate(ctx context.Context) error {
	_, err := d.ValidateIdentity(ctx)
	return err
}

// ValidateIdentity is Validate plus the bot user and its application id. A
// failed application lookup does not fail validation (the token is valid);
// ApplicationID is then empty and the invite step points at the portal.
func (d *DiscordAdapter) ValidateIdentity(ctx context.Context) (BotIdentity, error) {
	if d.config.BotToken == "" {
		return BotIdentity{}, fmt.Errorf("discord: bot_token is required")
	}
	if err := unresolvedTokenRef("discord", d.config.BotToken); err != nil {
		return BotIdentity{}, err
	}
	if err := ValidateDiscordChannelIDs(d.config.Channels); err != nil {
		return BotIdentity{}, err
	}
	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	}
	status, detail, err := d.restGet(ctx, "/users/@me", &me)
	if err != nil {
		return BotIdentity{}, err
	}
	switch {
	case status == http.StatusUnauthorized:
		return BotIdentity{}, fmt.Errorf("discord: bot token rejected (HTTP 401): %s — "+
			"copy it again from the Developer Portal → your app → Bot → Reset Token", detail)
	case status < 200 || status >= 300:
		return BotIdentity{}, fmt.Errorf("discord: GET /users/@me failed (HTTP %d): %s", status, detail)
	}
	if !me.Bot {
		return BotIdentity{}, fmt.Errorf("discord: this is not a bot token (it signs in as a user account) — " +
			"use the token from the Developer Portal → your app → Bot")
	}
	id := BotIdentity{ID: me.ID, Username: me.Username}

	var app struct {
		ID string `json:"id"`
	}
	if status, detail, err := d.restGet(ctx, "/oauth2/applications/@me", &app); err != nil || status < 200 || status >= 300 {
		slog.Debug("discord: application lookup failed; invite link needs the portal",
			"status", status, "detail", detail, "err", err)
	} else {
		id.ApplicationID = app.ID
	}
	return id, nil
}

// restGet performs one authenticated Discord REST GET. On a 2xx it decodes
// the body into out; otherwise it returns the status and Discord's error
// message. A returned error means the request never got a response. The
// token is only ever sent in the Authorization header, and no error built
// here quotes it.
func (d *DiscordAdapter) restGet(ctx context.Context, path string, out any) (int, string, error) {
	if err := ValidateAPIBase(d.apiBase); err != nil {
		return 0, "", fmt.Errorf("discord: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(d.apiBase, "/")+path, nil)
	if err != nil {
		return 0, "", fmt.Errorf("discord: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+d.config.BotToken)
	resp, err := d.apiClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, "", fmt.Errorf("discord: REST API unreachable: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, discordErrorMessage(body), nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, "", fmt.Errorf("discord: decode %s: %w", path, err)
	}
	return resp.StatusCode, "", nil
}

// discordErrorMessage extracts {"message": …} from a Discord REST error body.
func discordErrorMessage(body []byte) string {
	var parsed struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
		return parsed.Message
	}
	return graphErrorMessage(body) // shared snippet fallback
}

// Validate checks the WhatsApp Cloud credentials with a lightweight Graph GET
// on the configured phone_number_id — no webhook listener is bound. A 401/403
// is decoded as a rejected/expired token; other non-2xx statuses surface the
// Graph error message.
func (w *WhatsAppAdapter) Validate(ctx context.Context) error {
	// Relay mode holds no local credentials — rysh-server owns the Graph token and
	// validates it, and the connection id is resolved from the server at channel
	// start. There is nothing to probe here, so treat it as a non-binding pass
	// rather than failing on the absent api_key/phone that direct mode requires.
	if w.config.Relay {
		return nil
	}
	// The same gate Start applies, so a profile the adapter would refuse to
	// start (no app_secret) never checks as PASS.
	if err := whatsAppDirectConfigError(w.config); err != nil {
		return err
	}
	reqURL, err := w.graphURL(url.PathEscape(w.config.Phone))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("whatsapp: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.config.APIKey)
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp: Graph API unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	detail := graphErrorMessage(body)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("whatsapp: Graph token rejected (HTTP %d): %s", resp.StatusCode, detail)
	case http.StatusNotFound:
		return fmt.Errorf("whatsapp: phone_number_id %q not found (HTTP 404): %s", w.config.Phone, detail)
	default:
		return fmt.Errorf("whatsapp: Graph API error (HTTP %d): %s", resp.StatusCode, detail)
	}
}

// graphErrorMessage extracts the human-readable message from a Meta Graph API
// error body ({"error":{"message":…}}), falling back to a body snippet.
func graphErrorMessage(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		s = "(empty response body)"
	}
	return s
}
