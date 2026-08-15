// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// dialUpstream opens a NATS connection to the upstream server's workspace
// namespace (ws.{workspace}.*) via the transparent WebSocket proxy at
// /workspaces/{workspace}/nats. It centralizes the URL/scheme/proxy-path/auth
// setup shared by the share actors and the forge-share actor. The caller passes
// any extra options (reconnect/closed handlers); a connection_type=share query
// param keeps these lightweight connections from counting against session limits.
func dialUpstream(cfg config.UpstreamConfig, name string, opts ...nats.Option) (*nats.Conn, error) {
	rawURL := cfg.URL
	if rawURL == "" {
		return nil, fmt.Errorf("no upstream URL configured")
	}
	workspace := cfg.WorkspaceName()

	wsURL := rawURL
	if strings.HasPrefix(rawURL, "https://") {
		wsURL = "wss://" + strings.TrimPrefix(rawURL, "https://")
	} else if strings.HasPrefix(rawURL, "http://") {
		wsURL = "ws://" + strings.TrimPrefix(rawURL, "http://")
	}

	proxyPath := "/workspaces/" + workspace + "/nats?connection_type=share"
	if cfg.APIKey != "" {
		proxyPath += "&api_key=" + url.QueryEscape(cfg.APIKey)
	}

	maxReconnects := cfg.MaxReconnectAttempts
	if maxReconnects == 0 {
		maxReconnects = -1 // unlimited
	}

	base := []nats.Option{
		nats.Name(name),
		nats.ProxyPath(proxyPath),
		nats.MaxReconnects(maxReconnects),
		nats.Token(cfg.APIKey),
		// Bound the initial connect so a misconfigured / unreachable upstream fails
		// fast with a clear error instead of blocking the caller indefinitely.
		nats.Timeout(8 * time.Second),
		nats.RetryOnFailedConnect(false),
	}
	reconnectInterval := 5 * time.Second
	if d, err := time.ParseDuration(cfg.ReconnectInterval); err == nil {
		reconnectInterval = d
	}
	base = append(base, nats.ReconnectWait(reconnectInterval))
	base = append(base, opts...)

	return nats.Connect(wsURL, base...)
}
