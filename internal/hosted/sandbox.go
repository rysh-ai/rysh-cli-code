// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// rysh Sandboxes client (rr-order-154 C10): the /api/v1/workspaces/:wsID/
// sandboxes routes `rysh sandbox` calls. Same connection rules as the hosted
// runner client: https (or loopback http), the workspace key in
// Authorization: Bearer only, never in an error.

// Sandbox is the server's sandbox view.
type Sandbox struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Size      string    `json:"size"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	SSH       *struct {
		Host    string `json:"host"`
		Port    int    `json:"port"`
		User    string `json:"user"`
		HostKey string `json:"host_key"`
	} `json:"ssh,omitempty"`
}

// SandboxCreateOptions is what a caller may choose at create.
type SandboxCreateOptions struct {
	Name         string `json:"name,omitempty"`
	Size         string `json:"size,omitempty"`
	SSHPublicKey string `json:"ssh_public_key,omitempty"`
	LLMProvider  string `json:"llm_provider,omitempty"`
	LLMModel     string `json:"llm_model,omitempty"`
	LLMAPIKey    string `json:"llm_api_key,omitempty"` // request body only
	FromSnapshot string `json:"from_snapshot,omitempty"`
	// Environment, the sandbox's own variables and secrets, and its egress
	// policy (boat-secrets' environments). Secret values: request body only.
	Environment string            `json:"environment,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Secrets     []SandboxSecret   `json:"secrets,omitempty"`
	Egress      *SandboxEgress    `json:"egress,omitempty"`
}

// SandboxSecret is one secret value for a create: an env var, or a file at
// Path under the sandbox user's home.
type SandboxSecret struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Path  string `json:"path,omitempty"`
}

// SandboxEgress is an outbound policy: "open", or "allowlist" with rules.
type SandboxEgress struct {
	Mode  string              `json:"mode"`
	Rules []SandboxEgressRule `json:"rules,omitempty"`
}

type SandboxEgressRule struct {
	Host  string `json:"host"`
	Ports []int  `json:"ports"`
}

// SandboxExecRequest / SandboxExecResult mirror the exec route.
type SandboxExecRequest struct {
	Argv           []string `json:"argv"`
	Cwd            string   `json:"cwd,omitempty"`
	Stdin          string   `json:"stdin,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type SandboxExecResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (c *Client) sandboxesPath() string {
	return "/api/v1/workspaces/" + url.PathEscape(c.WorkspaceID) + "/sandboxes"
}

func (c *Client) sandboxPath(id, suffix string) (string, error) {
	if !runIDRe.MatchString(id) {
		return "", fmt.Errorf("%q is not a sandbox id", id)
	}
	p := c.sandboxesPath() + "/" + id
	if suffix != "" {
		p += "/" + suffix
	}
	return p, nil
}

func (c *Client) agentPath(id, pane, verb string) (string, error) {
	if !paneIDRe.MatchString(pane) {
		return "", fmt.Errorf("%q is not a pane id", pane)
	}
	return c.sandboxPath(id, "agents/"+pane+"/"+verb)
}

// SandboxCreate starts a sandbox.
func (c *Client) SandboxCreate(ctx context.Context, o SandboxCreateOptions) (Sandbox, error) {
	var sb Sandbox
	err := c.do(ctx, http.MethodPost, c.sandboxesPath(), o, http.Header{"Idempotency-Key": {idemKey()}}, &sb, o.LLMAPIKey)
	return sb, err
}

// SandboxList lists the workspace's sandboxes.
func (c *Client) SandboxList(ctx context.Context) ([]Sandbox, error) {
	var out struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	err := c.do(ctx, http.MethodGet, c.sandboxesPath(), nil, nil, &out)
	return out.Sandboxes, err
}

func (c *Client) sandboxCall(ctx context.Context, method, id, suffix string) (Sandbox, error) {
	p, err := c.sandboxPath(id, suffix)
	if err != nil {
		return Sandbox{}, err
	}
	var sb Sandbox
	err = c.do(ctx, method, p, nil, nil, &sb)
	return sb, err
}

func (c *Client) SandboxGet(ctx context.Context, id string) (Sandbox, error) {
	return c.sandboxCall(ctx, http.MethodGet, id, "")
}
func (c *Client) SandboxStop(ctx context.Context, id string) (Sandbox, error) {
	return c.sandboxCall(ctx, http.MethodPost, id, "stop")
}
func (c *Client) SandboxResume(ctx context.Context, id string) (Sandbox, error) {
	return c.sandboxCall(ctx, http.MethodPost, id, "resume")
}
func (c *Client) SandboxDelete(ctx context.Context, id string) (Sandbox, error) {
	return c.sandboxCall(ctx, http.MethodDelete, id, "")
}

// SandboxExec runs one command (argv, no shell unless argv names one).
func (c *Client) SandboxExec(ctx context.Context, id string, r SandboxExecRequest) (SandboxExecResult, error) {
	p, err := c.sandboxPath(id, "exec")
	if err != nil {
		return SandboxExecResult{}, err
	}
	var res SandboxExecResult
	err = c.do(ctx, http.MethodPost, p, r, nil, &res)
	return res, err
}

// SandboxPrompt / SandboxSteer / SandboxInterrupt drive one pane's agent.
func (c *Client) SandboxPrompt(ctx context.Context, id, pane, text string, waitSeconds int) (PromptResult, error) {
	p, err := c.agentPath(id, pane, "prompt")
	if err != nil {
		return PromptResult{}, err
	}
	var res PromptResult
	err = c.do(ctx, http.MethodPost, p, map[string]any{"text": text, "wait_seconds": waitSeconds}, nil, &res)
	return res, err
}

func (c *Client) SandboxSteer(ctx context.Context, id, pane, text string) error {
	p, err := c.agentPath(id, pane, "steer")
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, p, map[string]any{"text": text}, nil, nil)
}

func (c *Client) SandboxInterrupt(ctx context.Context, id, pane string) error {
	p, err := c.agentPath(id, pane, "interrupt")
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, p, nil, nil, nil)
}

// SandboxPanes lists the sandbox's panes.
func (c *Client) SandboxPanes(ctx context.Context, id string) ([]Pane, error) {
	p, err := c.sandboxPath(id, "panes")
	if err != nil {
		return nil, err
	}
	var out struct {
		Panes []Pane `json:"panes"`
	}
	err = c.do(ctx, http.MethodGet, p, nil, nil, &out)
	return out.Panes, err
}

// SandboxEvents follows the sandbox's event stream (see Events).
func (c *Client) SandboxEvents(ctx context.Context, id string, fn func(Event) error) error {
	p, err := c.sandboxPath(id, "events")
	if err != nil {
		return err
	}
	return c.streamEvents(ctx, p, fn)
}

// maxSandboxFile mirrors the server's file cap.
const maxSandboxFile = 16 << 20

// SandboxPutFile uploads data to path in the sandbox.
func (c *Client) SandboxPutFile(ctx context.Context, id, path string, data []byte) error {
	p, err := c.sandboxPath(id, "files")
	if err != nil {
		return err
	}
	_, err = c.doRaw(ctx, http.MethodPut, p+"?path="+url.QueryEscape(path), data)
	return err
}

// SandboxGetFile downloads path from the sandbox.
func (c *Client) SandboxGetFile(ctx context.Context, id, path string) ([]byte, error) {
	p, err := c.sandboxPath(id, "files")
	if err != nil {
		return nil, err
	}
	return c.doRaw(ctx, http.MethodGet, p+"?path="+url.QueryEscape(path), nil)
}

// doRaw sends body as application/octet-stream and returns the raw answer.
func (c *Client) doRaw(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if err := c.Conn.Validate(); err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/octet-stream")
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach the server: %s", scrub(err.Error(), []string{c.APIKey}))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSandboxFile+1))
	if err != nil {
		return nil, fmt.Errorf("could not read the answer: %s", scrub(err.Error(), []string{c.APIKey}))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, friendly(resp.StatusCode, scrub(e.Error, []string{c.APIKey}))
	}
	if len(raw) > maxSandboxFile {
		return nil, fmt.Errorf("the file is larger than %d bytes", maxSandboxFile)
	}
	return raw, nil
}

// SandboxPort is one port of a sandbox (C9 public URLs).
type SandboxPort struct {
	Port   int  `json:"port"`
	Public bool `json:"public"`
}

// SandboxPortURL is a short-lived URL for a public port.
type SandboxPortURL struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func validPort(p int) error {
	if p < 1 || p > 65535 || p == 22 {
		return fmt.Errorf("port %d: must be 1..65535 and not 22", p)
	}
	return nil
}

// SandboxPorts lists the sandbox's ports.
func (c *Client) SandboxPorts(ctx context.Context, id string) ([]SandboxPort, error) {
	p, err := c.sandboxPath(id, "ports")
	if err != nil {
		return nil, err
	}
	var out struct {
		Ports []SandboxPort `json:"ports"`
	}
	err = c.do(ctx, http.MethodGet, p, nil, nil, &out)
	return out.Ports, err
}

// SandboxSetPortPublic switches a port public or private.
func (c *Client) SandboxSetPortPublic(ctx context.Context, id string, port int, public bool) (SandboxPort, error) {
	if err := validPort(port); err != nil {
		return SandboxPort{}, err
	}
	p, err := c.sandboxPath(id, fmt.Sprintf("ports/%d", port))
	if err != nil {
		return SandboxPort{}, err
	}
	var out SandboxPort
	err = c.do(ctx, http.MethodPut, p, map[string]any{"public": public}, nil, &out)
	return out, err
}

// SandboxPortURL mints a short-lived URL for a public port.
func (c *Client) SandboxPortURL(ctx context.Context, id string, port int) (SandboxPortURL, error) {
	if err := validPort(port); err != nil {
		return SandboxPortURL{}, err
	}
	p, err := c.sandboxPath(id, fmt.Sprintf("ports/%d/url", port))
	if err != nil {
		return SandboxPortURL{}, err
	}
	var out SandboxPortURL
	err = c.do(ctx, http.MethodPost, p, nil, nil, &out)
	return out, err
}

// SandboxScreenshot returns the sandbox desktop as a PNG (C8).
func (c *Client) SandboxScreenshot(ctx context.Context, id string) ([]byte, error) {
	p, err := c.sandboxPath(id, "desktop/screenshot")
	if err != nil {
		return nil, err
	}
	return c.doRaw(ctx, http.MethodGet, p, nil)
}

// SandboxSnapshotInfo is one named snapshot (a template; boat-fork C3).
type SandboxSnapshotInfo struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	SourceSandboxID string    `json:"source_sandbox_id"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

var snapshotIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// SandboxFork makes count forks (1..10) of a running sandbox.
func (c *Client) SandboxFork(ctx context.Context, id string, count int, sshPublicKey string) ([]Sandbox, error) {
	if count < 1 || count > 10 {
		return nil, fmt.Errorf("fork count %d: must be 1..10", count)
	}
	p, err := c.sandboxPath(id, "fork")
	if err != nil {
		return nil, err
	}
	body := map[string]any{"count": count}
	if sshPublicKey != "" {
		body["ssh_public_key"] = sshPublicKey
	}
	var out struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	err = c.do(ctx, http.MethodPost, p, body, nil, &out)
	return out.Sandboxes, err
}

// SandboxSnapshot saves a named snapshot of a sandbox.
func (c *Client) SandboxSnapshot(ctx context.Context, id, name string) (SandboxSnapshotInfo, error) {
	p, err := c.sandboxPath(id, "snapshots")
	if err != nil {
		return SandboxSnapshotInfo{}, err
	}
	var out SandboxSnapshotInfo
	err = c.do(ctx, http.MethodPost, p, map[string]any{"name": name}, nil, &out)
	return out, err
}

// SandboxSnapshots lists the workspace's snapshots.
func (c *Client) SandboxSnapshots(ctx context.Context) ([]SandboxSnapshotInfo, error) {
	var out struct {
		Snapshots []SandboxSnapshotInfo `json:"snapshots"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/workspaces/"+url.PathEscape(c.WorkspaceID)+"/snapshots", nil, nil, &out)
	return out.Snapshots, err
}

// SandboxDeleteSnapshot deletes one snapshot.
func (c *Client) SandboxDeleteSnapshot(ctx context.Context, snapshotID string) error {
	if !snapshotIDRe.MatchString(snapshotID) {
		return fmt.Errorf("%q is not a snapshot id", snapshotID)
	}
	return c.do(ctx, http.MethodDelete, "/api/v1/workspaces/"+url.PathEscape(c.WorkspaceID)+"/snapshots/"+snapshotID, nil, nil, nil)
}

// SandboxDesktopURL asks for a desktop embed (C8, boat-desktop) and returns
// the viewer URL. The one-time token rides in the URL fragment (after '#'),
// which browsers never send to a server, so it stays out of every log.
func (c *Client) SandboxDesktopURL(ctx context.Context, id, mode string) (string, error) {
	if mode != "watch" && mode != "control" {
		return "", fmt.Errorf("desktop mode %q: watch or control", mode)
	}
	p, err := c.sandboxPath(id, "desktop/embeds")
	if err != nil {
		return "", err
	}
	var out struct {
		Path     string `json:"path"`
		Fragment string `json:"fragment"`
	}
	if err := c.do(ctx, http.MethodPost, p, map[string]any{"mode": mode}, nil, &out); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out.Path, "/") || strings.Contains(out.Path, "//") {
		return "", fmt.Errorf("the server answered with an unusable embed path")
	}
	return strings.TrimRight(c.URL, "/") + out.Path + "#" + out.Fragment, nil
}

// SandboxEnvironmentInfo is one version of an environment (secret names only).
type SandboxEnvironmentInfo struct {
	Name    string            `json:"name"`
	Version int               `json:"version"`
	Vars    map[string]string `json:"vars,omitempty"`
	Secrets []struct {
		Name string `json:"name"`
		Path string `json:"path,omitempty"`
	} `json:"secrets,omitempty"`
	SetupScript string         `json:"setup_script,omitempty"`
	Egress      *SandboxEgress `json:"egress,omitempty"`
	CreatedBy   string         `json:"created_by,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

var envNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func (c *Client) envPath(name string) (string, error) {
	base := "/api/v1/workspaces/" + url.PathEscape(c.WorkspaceID) + "/environments"
	if name == "" {
		return base, nil
	}
	return envItemPath(base, name)
}

func envItemPath(base, name string) (string, error) {
	if !envNameRe.MatchString(name) {
		return "", fmt.Errorf("%q is not an environment name (a-z, 0-9, '-'; up to 32)", name)
	}
	return base + "/" + name, nil
}

// SandboxEnvironments lists the workspace's environments (latest of each).
func (c *Client) SandboxEnvironments(ctx context.Context) ([]SandboxEnvironmentInfo, error) {
	p, _ := c.envPath("")
	var out struct {
		Environments []SandboxEnvironmentInfo `json:"environments"`
	}
	err := c.do(ctx, http.MethodGet, p, nil, nil, &out)
	return out.Environments, err
}

// SandboxEnvironment gets one environment (version 0 = latest).
func (c *Client) SandboxEnvironment(ctx context.Context, name string, version int) (SandboxEnvironmentInfo, error) {
	p, err := envItemPath("/api/v1/workspaces/"+url.PathEscape(c.WorkspaceID)+"/environments", name)
	if err != nil {
		return SandboxEnvironmentInfo{}, err
	}
	if version > 0 {
		p += "?version=" + strconv.Itoa(version)
	}
	var out SandboxEnvironmentInfo
	err = c.do(ctx, http.MethodGet, p, nil, nil, &out)
	return out, err
}

// SandboxEnvironmentPut saves body (a JSON object: vars, secrets by name,
// setup_script, egress, safe_for_third_parties) as a new version, or creates
// the environment when it does not exist yet.
func (c *Client) SandboxEnvironmentPut(ctx context.Context, name string, body []byte) (SandboxEnvironmentInfo, error) {
	p, err := envItemPath("/api/v1/workspaces/"+url.PathEscape(c.WorkspaceID)+"/environments", name)
	if err != nil {
		return SandboxEnvironmentInfo{}, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return SandboxEnvironmentInfo{}, errors.New("the environment file must hold one JSON object")
	}
	var out SandboxEnvironmentInfo
	err = c.do(ctx, http.MethodPut, p, json.RawMessage(body), nil, &out)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		return out, err
	}
	n, _ := json.Marshal(name)
	obj["name"] = n
	base, _ := c.envPath("")
	err = c.do(ctx, http.MethodPost, base, obj, nil, &out)
	return out, err
}

// SandboxEnvironmentDelete deletes every version of an environment.
func (c *Client) SandboxEnvironmentDelete(ctx context.Context, name string) error {
	p, err := envItemPath("/api/v1/workspaces/"+url.PathEscape(c.WorkspaceID)+"/environments", name)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, p, nil, nil, nil)
}
