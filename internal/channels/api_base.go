// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateAPIBase checks a ChannelConfig.APIBase override (gap05, G13): the
// API root an adapter talks to instead of the platform's production endpoint —
// a self-hosted Telegram Bot API server, or a local fake platform in tests.
// Empty means "use production" and is always valid.
//
// A bot token rides in every request (in the URL path on Telegram), so a
// non-empty value must be https, or plain http only on a loopback host. The
// error never quotes the value, so it cannot echo a credential pasted into
// the wrong field.
func ValidateAPIBase(raw string) error { return validateBaseURL("api_base", raw) }

// ValidateServerURL is ValidateAPIBase for a server_url field (Mattermost,
// Matrix homeserver), where the value is required: empty is refused.
func ValidateServerURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("server_url is required")
	}
	return validateBaseURL("server_url", raw)
}

// validateBaseURL is the shared check; field names the config key in errors.
func validateBaseURL(field, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.Contains(raw, "${") {
		return fmt.Errorf("%s is an unresolved ${…} reference", field)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%s must be an absolute URL such as https://host[:port]", field)
	}
	if u.User != nil {
		return fmt.Errorf("%s must not carry user info", field)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%s must use https (plain http is allowed only on a loopback host)", field)
	default:
		return fmt.Errorf("%s scheme must be https (or http on a loopback host)", field)
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
