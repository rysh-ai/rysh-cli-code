// SPDX-License-Identifier: Apache-2.0

package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	pc "github.com/rysh-ai/rysh-cli-shared/peercoord"
)

// Coordination uses the existing workspace authorization plus a registration
// credential. Credentials stay in the local daemon, not task prompts.
type CoordinationClient struct {
	Client
	PeerToken string
}

func (c CoordinationClient) call(ctx context.Context, method, suffix, executionToken string, body, out any) error {
	if err := ValidateURL(c.Connection.URL); err != nil {
		return err
	}
	if c.Key == "" {
		return fmt.Errorf("peer API key secret is unavailable")
	}
	path, err := c.path()
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Connection.URL+path+"/coordination"+suffix, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Rysh-Peer-Token", c.PeerToken)
	req.Header.Set("X-Rysh-Execution-Token", executionToken)
	h := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := h.Do(req)
	if err != nil {
		return fmt.Errorf("peer server unreachable; delivery unconfirmed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var remote struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&remote)
		if remote.Code != "" {
			return pc.Fail(remote.Code, "%s", Display(remote.Error))
		}
		return pc.Fail("http", "peer server returned HTTP %d", resp.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("invalid coordination response")
	}
	return nil
}
func (c CoordinationClient) State(ctx context.Context) (pc.State, error) {
	var s pc.State
	err := c.call(ctx, "GET", "", "", nil, &s)
	return s, err
}
func (c CoordinationClient) Apply(ctx context.Context, r pc.Request, token string) (pc.Response, error) {
	var s pc.Response
	err := c.call(ctx, "POST", "/commands", token, r, &s)
	return s, err
}
func (c CoordinationClient) Events(ctx context.Context, after int64) (pc.Page, error) {
	var s pc.Page
	err := c.call(ctx, "GET", fmt.Sprintf("/events?after=%d&limit=100", after), "", nil, &s)
	return s, err
}
