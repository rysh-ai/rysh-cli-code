package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	if s.config.BotToken == "" {
		return fmt.Errorf("slack: bot_token is required")
	}
	if s.config.AppToken == "" {
		return fmt.Errorf("slack: app_token is required")
	}
	opts := []slack.Option{}
	if slackValidateAPIURL != "" {
		opts = append(opts, slack.OptionAPIURL(slackValidateAPIURL))
	}
	api := slack.New(s.config.BotToken, opts...)
	if _, err := api.AuthTestContext(ctx); err != nil {
		return fmt.Errorf("slack: auth.test failed: %w", err)
	}
	if !strings.HasPrefix(s.config.AppToken, "xapp-") {
		return fmt.Errorf("slack: app_token does not look like an app-level token (expected xapp-…)")
	}
	return nil
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
	if w.config.APIKey == "" {
		return fmt.Errorf("whatsapp: api_key (Graph access token) is required")
	}
	if w.config.Phone == "" {
		return fmt.Errorf("whatsapp: phone (phone_number_id) is required")
	}
	reqURL := fmt.Sprintf("%s/%s/%s", w.baseURL, w.graphVer, url.PathEscape(w.config.Phone))
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
