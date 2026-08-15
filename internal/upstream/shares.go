// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// RemoteShare represents a shared entity returned by the upstream server.
type RemoteShare struct {
	ID          string `json:"id"`
	ShareID     string `json:"share_id"`
	EntityType  string `json:"entity_type"`
	EntityID    string `json:"entity_id"`
	EntityAlias string `json:"entity_alias"`
	ShareMode   string `json:"share_mode"`
	State       string `json:"state"`
	Origin      string `json:"origin"`
	SessionName string `json:"session_name"`
	CreatedAt   string `json:"created_at"`
}

// FetchRemoteShares retrieves all active shares in the workspace from the
// upstream server. The workspace API key scopes the query to the key's
// workspace only.
func FetchRemoteShares(cfg config.Config) ([]RemoteShare, error) {
	if !cfg.Upstream.Enabled || cfg.Upstream.URL == "" {
		return nil, fmt.Errorf("upstream not configured")
	}
	if cfg.Upstream.APIKey == "" {
		return nil, fmt.Errorf("upstream api_key not set")
	}

	wsName := cfg.Upstream.WorkspaceName()
	url := strings.TrimRight(cfg.Upstream.URL, "/") +
		fmt.Sprintf("/api/workspaces/%s/shares/list", wsName)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Upstream.APIKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch remote shares: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch remote shares: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Shares []RemoteShare `json:"shares"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode remote shares: %w", err)
	}
	return result.Shares, nil
}
