// Package limits provides subscription resource limit checking for the rysh daemon.
// When upstream is enabled and an API key is configured, the daemon fetches
// the user's subscription limits from the server and caches them. Before creating
// panes, the WorkspaceActor calls CheckCreate to validate that the creation
// is permitted under the current subscription plan.
//
// When upstream is not enabled (local-only mode), all operations are allowed.
package limits

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ResourceLimits mirrors the server's UserLimits response.
type ResourceLimits struct {
	PlanID        string `json:"plan_id"`
	PlanName      string `json:"plan_name"`
	MaxWorkspaces int    `json:"max_workspaces"`
	MaxSessions   int    `json:"max_sessions"`
	MaxPanes      int    `json:"max_panes"`
	// UpgradeURL is where the server wants a user who just hit a limit to go.
	// It may be relative to the server (the default) or absolute; resolve it
	// with resolveUpgradeURL before showing it. Empty means the deployment
	// configured none, and nothing is shown — a self-hosted rysh must never
	// advertise somebody else's billing page.
	UpgradeURL string `json:"upgrade_url,omitempty"`
}

// ResourceUsage describes the current resource counts in the workspace.
type ResourceUsage struct {
	Panes int `json:"panes"`
}

// LimitCheckResult mirrors the server's check-limits response.
type LimitCheckResult struct {
	Allowed  bool   `json:"allowed"`
	Resource string `json:"resource,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Current  int    `json:"current,omitempty"`
	Error    string `json:"error,omitempty"`
	PlanID   string `json:"plan_id"`
	PlanName string `json:"plan_name"`
	// UpgradeURL — see ResourceLimits.UpgradeURL.
	UpgradeURL string `json:"upgrade_url,omitempty"`
}

// resolveUpgradeURL turns whatever the server sent into something a user can
// actually paste into a browser, or into "" if there is nothing to show.
//
// The server's default is deployment-relative ("/pricing"), so the CLI joins it
// onto the server it is talking to. That is what keeps a self-hoster's terminal
// pointing at their own deployment. An absolute URL is passed through; a
// relative one with no server URL to hang it on yields "" rather than a
// fragment that looks like a link but is not one.
func resolveUpgradeURL(serverURL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	base := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if base == "" {
		return ""
	}
	return base + "/" + strings.TrimLeft(raw, "/")
}

// upgradeHint renders the trailing clause appended to a limit message, or ""
// when there is no URL to offer. Mirrors the server-side helper of the same
// name so both surfaces read identically.
func (c *Checker) upgradeHint(raw string) string {
	if u := resolveUpgradeURL(c.serverURL, raw); u != "" {
		return " — upgrade at " + u
	}
	return ""
}

// Checker validates resource limits against the server's subscription system.
// It caches limits locally and refreshes them periodically.
type Checker struct {
	serverURL string
	apiKey    string
	client    *http.Client

	mu        sync.RWMutex
	cached    *ResourceLimits
	lastFetch time.Time

	// refreshInterval controls how often limits are re-fetched from the server.
	refreshInterval time.Duration
}

// NewChecker creates a Checker that talks to the given server URL with the
// given API key. If serverURL or apiKey is empty, the checker is a no-op
// (all operations are allowed).
func NewChecker(serverURL, apiKey string) *Checker {
	return &Checker{
		serverURL: serverURL,
		apiKey:    apiKey,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		refreshInterval: 5 * time.Minute,
	}
}

// Enabled returns true if the checker is configured to enforce limits.
func (c *Checker) Enabled() bool {
	return c.serverURL != "" && c.apiKey != ""
}

// FetchLimits synchronously fetches the user's resource limits from the server.
// This should be called once during daemon startup.
func (c *Checker) FetchLimits() error {
	if !c.Enabled() {
		return nil
	}

	limits, err := c.doFetchLimits()
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.cached = limits
	c.lastFetch = time.Now()
	c.mu.Unlock()

	slog.Info("subscription limits loaded",
		"plan", limits.PlanName,
		"max_sessions", limits.MaxSessions,
		"max_panes", limits.MaxPanes,
	)

	return nil
}

// CachedLimits returns the cached limits, or nil if not yet fetched.
func (c *Checker) CachedLimits() *ResourceLimits {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cached
}

// CheckCreate validates whether a resource creation is allowed given the
// current usage plus the resources about to be added. Returns nil if allowed,
// or an error describing which limit was exceeded.
//
// Parameters:
//   - usage: current resource counts in the workspace
//   - addPanes: panes being added
func (c *Checker) CheckCreate(usage ResourceUsage, addPanes int) error {
	if !c.Enabled() {
		return nil
	}
	c.refreshIfStale()
	c.mu.RLock()
	limits := c.cached
	c.mu.RUnlock()
	if limits == nil {
		return nil
	}
	if limits.MaxPanes > 0 && usage.Panes+addPanes > limits.MaxPanes {
		return fmt.Errorf("pane limit reached (%d/%d on %s plan). Upgrade your plan to create more panes%s",
			usage.Panes, limits.MaxPanes, limits.PlanName, c.upgradeHint(limits.UpgradeURL))
	}
	return nil
}

// ServerCheckCreate performs a server-side limit check via HTTP POST.
// This is used for authoritative validation (e.g., before actually spawning actors).
func (c *Checker) ServerCheckCreate(usage ResourceUsage) (*LimitCheckResult, error) {
	if !c.Enabled() {
		return &LimitCheckResult{Allowed: true}, nil
	}

	body, err := json.Marshal(usage)
	if err != nil {
		return nil, fmt.Errorf("marshal usage: %w", err)
	}

	url := c.serverURL + "/api/resource/check-limits"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("check limits request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result LimitCheckResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &result, nil
}

// doFetchLimits makes an HTTP GET request to the server to fetch limits.
func (c *Checker) doFetchLimits() (*ResourceLimits, error) {
	url := c.serverURL + "/api/resource/limits"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch limits: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch limits: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var limits ResourceLimits
	if err := json.NewDecoder(resp.Body).Decode(&limits); err != nil {
		return nil, fmt.Errorf("decode limits: %w", err)
	}

	return &limits, nil
}

// refreshIfStale checks if the cached limits are stale and refreshes them
// in the background if needed. This is non-blocking.
func (c *Checker) refreshIfStale() {
	c.mu.RLock()
	stale := time.Since(c.lastFetch) > c.refreshInterval
	c.mu.RUnlock()

	if !stale {
		return
	}

	// Refresh in background to avoid blocking the actor.
	go func() {
		limits, err := c.doFetchLimits()
		if err != nil {
			slog.Warn("failed to refresh subscription limits", "err", err)
			return
		}
		c.mu.Lock()
		c.cached = limits
		c.lastFetch = time.Now()
		c.mu.Unlock()
		slog.Debug("subscription limits refreshed", "plan", limits.PlanName)
	}()
}
