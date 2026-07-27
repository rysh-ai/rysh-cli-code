package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// uuidRe matches a canonical UUID (the form workspace ids take).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// FetchWorkspaceID queries the upstream server's /api/server-info endpoint with
// the workspace API key and returns the workspace id (UUID). The id is the
// globally unique NATS namespace token (ws.{id}.*); workspace names are unique
// per user, so the name cannot be used on the wire.
func FetchWorkspaceID(up config.UpstreamConfig) (string, error) {
	if up.URL == "" {
		return "", fmt.Errorf("upstream url not set")
	}
	if up.APIKey == "" {
		return "", fmt.Errorf("upstream api_key not set")
	}

	url := strings.TrimRight(up.URL, "/") + "/api/server-info"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("X-API-Key", up.APIKey)

	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch server-info: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch server-info: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Workspace   string `json:"workspace"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode server-info: %w", err)
	}
	if result.WorkspaceID == "" {
		return "", fmt.Errorf("server-info did not return a workspace_id (server may predate per-user workspace names)")
	}
	return result.WorkspaceID, nil
}

// ResolveWorkspaceID returns the upstream NATS namespace token for the given
// upstream config. When the configured workspace value is already a UUID it is
// returned unchanged (no network call). Otherwise — when sharing is enabled and
// an api_key is present — it resolves the workspace id from the server via the
// api key. On any failure it falls back to the configured value so the CLI still
// starts (sharing simply won't line up until the id is correct).
func ResolveWorkspaceID(up config.UpstreamConfig) string {
	current := up.WorkspaceName()
	if uuidRe.MatchString(current) {
		return current // already a workspace id
	}
	if !up.Enabled || up.URL == "" || up.APIKey == "" {
		return current
	}
	id, err := FetchWorkspaceID(up)
	if err != nil {
		slog.Warn("upstream: could not resolve workspace id from api key; "+
			"set [upstream] workspace to the workspace UUID from the dashboard",
			"configured", current, "err", err)
		return current
	}
	slog.Info("upstream: resolved workspace id from api key", "workspace_id", id)
	return id
}
