// Package upstream provides HTTP helpers for fetching workspace-scoped data
// from the rysh-server using the local upstream API key.
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

// ConnectionCredentials carries decrypted secrets plus public config for an
// external connection of a given type, fetched from the rysh-server.
type ConnectionCredentials struct {
	ConnectionID   string            `json:"connection_id"`
	WorkspaceID    string            `json:"workspace_id"`
	Name           string            `json:"name"`
	ConnectionType string            `json:"connection_type"`
	Config         map[string]string `json:"config"`
	Secrets        map[string]string `json:"secrets"`
}

// FetchConnectionCredentials retrieves the decrypted credentials for the
// workspace's enabled connection of the given type (e.g. "slack", "whatsapp").
// Returns an error if upstream is not configured, the request fails, or the
// workspace has no enabled connection of that type.
func FetchConnectionCredentials(cfg config.Config, connType string) (*ConnectionCredentials, error) {
	if !cfg.Upstream.Enabled || cfg.Upstream.URL == "" {
		return nil, fmt.Errorf("upstream not configured")
	}
	if cfg.Upstream.APIKey == "" {
		return nil, fmt.Errorf("upstream api_key not set")
	}

	wsName := cfg.Upstream.WorkspaceName()
	url := strings.TrimRight(cfg.Upstream.URL, "/") +
		fmt.Sprintf("/api/workspaces/%s/connections/by-type/%s/credentials", wsName, connType)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Upstream.APIKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch credentials: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch credentials: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var creds ConnectionCredentials
	if err := json.Unmarshal(body, &creds); err != nil {
		return nil, fmt.Errorf("decode credentials: %w", err)
	}
	return &creds, nil
}
