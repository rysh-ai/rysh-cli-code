// SPDX-License-Identifier: Apache-2.0

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

// WorkspaceInfo is what /api/server-info tells a client about the workspace
// its API key belongs to: the Name a human recognises, and the ID that is the
// only thing usable on the wire.
type WorkspaceInfo struct {
	Name string
	ID   string
}

// FetchWorkspaceInfo queries the upstream server's /api/server-info endpoint
// with the workspace API key and returns the workspace name and id (UUID). The
// id is the globally unique NATS namespace token (ws.{id}.*); workspace names
// are unique per user, so the name cannot be used on the wire.
//
// Its errors are written for a human to read as one line — `##upstream connect`
// prints them verbatim. A user who mistyped a URL should see that, not a
// wrapped *url.Error with an HTTP body glued to the end of it.
func FetchWorkspaceInfo(up config.UpstreamConfig) (WorkspaceInfo, error) {
	if up.URL == "" {
		return WorkspaceInfo{}, fmt.Errorf("upstream url not set")
	}
	if up.APIKey == "" {
		return WorkspaceInfo{}, fmt.Errorf("upstream api_key not set")
	}

	base := strings.TrimRight(up.URL, "/")
	req, err := http.NewRequest(http.MethodGet, base+"/api/server-info", nil)
	if err != nil {
		return WorkspaceInfo{}, fmt.Errorf("could not reach %s: %w", base, err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("X-API-Key", up.APIKey)

	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Transport-level: DNS, refused connection, TLS, timeout. The URL is
		// what the user can act on, so lead with it.
		return WorkspaceInfo{}, fmt.Errorf("could not reach %s — check the URL and that the server is running", base)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return WorkspaceInfo{}, fmt.Errorf("%s rejected that api key (HTTP %d)", base, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return WorkspaceInfo{}, fmt.Errorf("%s returned HTTP %d — is that a rysh server?", base, resp.StatusCode)
	}

	var result struct {
		Workspace   string `json:"workspace"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return WorkspaceInfo{}, fmt.Errorf("%s did not return valid server-info JSON — is that a rysh server?", base)
	}
	if result.WorkspaceID == "" {
		// Never guess a namespace here: a session that connects to the wrong
		// one silently sees no shares, which is worse than refusing.
		return WorkspaceInfo{}, fmt.Errorf("%s returned no workspace_id — the api key may be unrecognised, or the server predates per-user workspace ids", base)
	}
	return WorkspaceInfo{Name: result.Workspace, ID: result.WorkspaceID}, nil
}

// FetchWorkspaceID is FetchWorkspaceInfo when only the namespace token matters.
func FetchWorkspaceID(up config.UpstreamConfig) (string, error) {
	info, err := FetchWorkspaceInfo(up)
	if err != nil {
		return "", err
	}
	return info.ID, nil
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
