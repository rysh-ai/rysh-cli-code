// SPDX-License-Identifier: Apache-2.0

package limits

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// sessionCheckTimeout bounds SessionLimitWarning. It runs on the workspace
// actor's mailbox for ##upstream status and connect, so a slow server must not
// hold every pane's ## commands for the client's normal 10 s.
const sessionCheckTimeout = 3 * time.Second

// SessionLimitWarning asks the server whether this account is over its plan's
// session cap (GET /api/resource/check-session-limit) and returns one line
// naming the limit and where to upgrade — or "" when there is nothing to say.
//
// It only informs. The server does not refuse a rysh daemon for the session
// cap (every daemon dial is connection_type=share or channel, which the proxy
// exempts), so an over-cap account keeps working and, before this, was never
// told. Anything other than a 402 with a message — within cap, an older server
// without the endpoint, an error, a timeout — yields "" and never an error:
// a status command must not fail because a billing hint could not be fetched.
func (c *Checker) SessionLimitWarning() string {
	if !c.Enabled() {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionCheckTimeout)
	defer cancel()

	url := strings.TrimRight(c.serverURL, "/") + "/api/resource/check-session-limit"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		return ""
	}

	var body struct {
		Error      string `json:"error"`
		UpgradeURL string `json:"upgrade_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return ""
	}
	msg := strings.TrimSpace(body.Error)
	if msg == "" {
		return ""
	}
	// The server already ends the sentence with " — upgrade at <upgrade_url>",
	// relative to itself. Swap that for the resolved link rather than print the
	// clause twice.
	raw := strings.TrimSpace(body.UpgradeURL)
	if raw != "" {
		msg = strings.TrimSuffix(msg, " — upgrade at "+raw)
	}
	return msg + c.upgradeHint(raw)
}
