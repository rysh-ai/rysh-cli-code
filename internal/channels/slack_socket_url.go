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

// slackHTTPDoer is the request side of slack-go's HTTP client option.
type slackHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// slackSocketURLGuard wraps the Slack API client's HTTP client and checks the
// Socket Mode URL apps.connections.open returns before slack-go dials it:
// wss://, or plain ws:// only to a loopback host (the local fake platform).
// Every other call passes through untouched.
type slackSocketURLGuard struct {
	next slackHTTPDoer
}

func (g slackSocketURLGuard) Do(req *http.Request) (*http.Response, error) {
	resp, err := g.next.Do(req)
	if err != nil || !strings.HasSuffix(req.URL.Path, "/apps.connections.open") {
		return resp, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var open struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(body, &open) == nil && open.URL != "" {
		if err := checkSlackSocketURL(open.URL); err != nil {
			return nil, err
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// checkSlackSocketURL refuses a Socket Mode URL that is not wss:// (plain
// ws:// only on a loopback host). The error names the scheme and host only:
// the URL's query carries the connection ticket.
func checkSlackSocketURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("slack: apps.connections.open returned a Socket Mode URL that does not parse; Socket Mode connects only over wss://")
	}
	switch {
	case u.Scheme == "wss":
		return nil
	case u.Scheme == "ws" && isLoopbackHost(u.Hostname()):
		return nil
	}
	return fmt.Errorf("slack: apps.connections.open returned a %s:// Socket Mode URL for host %q; Socket Mode connects only over wss:// (plain ws:// only to a loopback test server) — check api_base",
		u.Scheme, u.Hostname())
}
