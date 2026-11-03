// SPDX-License-Identifier: Apache-2.0

package familyclient

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateServer checks a family server URL before any password or token is
// sent to it. Both travel in every request, so the URL must be https (wss for
// the socket), or plain http/ws only on a loopback host — the rule
// channels.ValidateAPIBase applies to a channel's api_base. It must be
// absolute and carry no user info. Errors name the host, never the whole URL.
func ValidateServer(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("no family server URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return errors.New("the family server must be an absolute URL such as https://host[:port]")
	}
	if u.User != nil {
		return fmt.Errorf("the family server URL for %s must not carry user info", u.Hostname())
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		return nil
	case "http", "ws":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("the family server %s must use https: plain http would send your password and tokens unencrypted (it is allowed only on a loopback host)", u.Hostname())
	default:
		return fmt.Errorf("the family server %s must use https (or http on a loopback host)", u.Hostname())
	}
}

// ServerHost is the host[:port] of a server URL, for telling the user where a
// secret is about to go.
func ServerHost(raw string) string {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
