// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// discordAPIBaseTransport sends discordgo's REST calls to ChannelConfig.APIBase
// instead of https://discord.com (gap05). discordgo's endpoints are package
// globals, so the session's HTTP client rewrites each request rather than the
// adapter mutating shared state. It also checks the Gateway URL that the
// api_base server hands back: IDENTIFY carries the bot token over that
// WebSocket, so it must be wss, or ws only on a loopback host.
type discordAPIBaseTransport struct {
	base *url.URL
	next http.RoundTripper
}

func newDiscordAPIBaseTransport(apiBase string, next http.RoundTripper) (*discordAPIBaseTransport, error) {
	if err := ValidateAPIBase(apiBase); err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(apiBase), "/"))
	if err != nil {
		return nil, fmt.Errorf("api_base: %w", err)
	}
	if next == nil {
		next = http.DefaultTransport
	}
	return &discordAPIBaseTransport{base: u, next: next}, nil
}

func (t *discordAPIBaseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.EqualFold(r.URL.Hostname(), "discord.com") {
		// Never let an unexpected host through this client: everything the
		// adapter sends goes to discord.com, i.e. to the api_base.
		return nil, fmt.Errorf("discord: api_base transport refuses a request to %s", r.URL.Hostname())
	}
	out := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = t.base.Scheme
	u.Host = t.base.Host
	u.Path = t.base.Path + r.URL.Path
	u.RawPath = ""
	out.URL = &u
	out.Host = ""
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	if p := strings.TrimSuffix(r.URL.Path, "/"); strings.HasSuffix(p, "/gateway") || strings.HasSuffix(p, "/gateway/bot") {
		body, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		var g struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(body, &g)
		if err := checkDiscordGatewayURL(g.URL); err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	return resp, nil
}

// checkDiscordGatewayURL refuses a Gateway URL the token could leak over.
func checkDiscordGatewayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("discord: api_base returned no usable gateway URL")
	}
	switch {
	case u.Scheme == "wss":
		return nil
	case u.Scheme == "ws" && isLoopbackHost(u.Hostname()):
		return nil
	}
	return fmt.Errorf("discord: api_base returned a gateway URL that is not wss (ws is allowed only on a loopback host)")
}
