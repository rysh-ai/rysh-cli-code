// SPDX-License-Identifier: Apache-2.0

// Package hosted is the laptop side of the rysh.ai hosted runner (design 074,
// "Hosted runner"): start a run on the server with the workspace API key,
// keep a throwaway ssh key and the pinned host key for it, and hand the
// ssh_config to `##remote add`. The server side is rysh-server
// internal/hostedrunner; this package changes nothing there.
//
// Secrets: the workspace API key and the optional LLM key go in request
// headers or the request body only, never in an error, a log line, a file in
// the run directory or a command line.
package hosted

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHUser is the unprivileged account inside every runner container.
const SSHUser = "rysh"

// SessionName and WorkDir are what the runner image creates (deploy/runner).
// WorkDir is relative to the SSH user's home (/home/rysh in the image): an ssh
// command starts there, so `cd work` lands in the same directory, and the
// binary carries no /home/<name>/ path for the release path guard to refuse.
const (
	SessionName = "work"
	WorkDir     = "work"
)

var (
	runIDRe   = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	hostKeyRe = regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/=]{20,200}( [^\r\n]*)?$`)
)

// Run is the server's run record (rysh-server hostedrunner.Run).
type Run struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	Source      string `json:"source,omitempty"`
	Host        string `json:"host,omitempty"`
	Port        int    `json:"port,omitempty"`
	HostKey     string `json:"host_key,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Size        string `json:"size,omitempty"` // small|default|large|xlarge; empty = the classic run
	// ForkOf is the run this one was forked from (##hosted fork).
	ForkOf    string    `json:"fork_of,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Conn is the server connection, normally read from the `upstream:` block of
// rysh.config.yaml.
type Conn struct {
	URL         string
	APIKey      string
	WorkspaceID string
}

// Validate refuses a connection that would send the key in clear text.
func (c Conn) Validate() error {
	if c.APIKey == "" {
		return errors.New("no workspace api key (set upstream.api_key in rysh.config.yaml, or run ##upstream connect <url> <api-key>)")
	}
	if c.WorkspaceID == "" {
		return errors.New("no workspace id (set upstream.workspace_id in rysh.config.yaml)")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" {
		return errors.New("no server url (set upstream.url in rysh.config.yaml)")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return nil
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New("the server url must be https (the api key would travel in clear text)")
}

// StartOptions are what a user may choose; the server fixes everything else.
type StartOptions struct {
	Name        string
	Prompt      string
	LLMProvider string // anthropic | gemini | openai
	LLMModel    string
	LLMKey      string // sent in the request body only
	// FromSnapshot starts the run from a named snapshot (##hosted up --from).
	FromSnapshot string
}

// Client talks to the hosted-runs routes.
type Client struct {
	Conn
	HTTP *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 90 * time.Second}
}

// APIError is a failed call, with a message that is safe to show.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func friendly(status int, serverMsg string) *APIError {
	switch status {
	case http.StatusUnauthorized:
		return &APIError{status, "the server refused the api key (expired or revoked; check upstream.api_key)"}
	case http.StatusForbidden:
		return &APIError{status, "this api key is read-only; the hosted runner needs a key with write permission"}
	case http.StatusNotFound:
		return &APIError{status, "the hosted runner is not enabled for this workspace (or that run does not exist)"}
	case http.StatusTooManyRequests:
		return &APIError{status, "run quota reached; stop a run first (##hosted list, ##hosted stop <id>)"}
	case http.StatusBadRequest:
		// The server's 400 text names the field that is wrong; it never holds a
		// key. Drop it anyway if it echoes anything we sent.
		return &APIError{status, "the server refused the request: " + clip(serverMsg, 200)}
	}
	return &APIError{status, fmt.Sprintf("could not complete the request (HTTP %d)", status)}
}

func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (c *Client) do(ctx context.Context, method, path string, body any, extra http.Header, out any, secrets ...string) error {
	if err := c.Conn.Validate(); err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey) // the workspace routes read a rysh_ key from Authorization, not X-API-Key
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header[k] = v
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the server: %s", scrub(err.Error(), append(secrets, c.APIKey)))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return friendly(resp.StatusCode, scrub(e.Error, append(secrets, c.APIKey)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("the server answered with something that is not a run record")
	}
	return nil
}

// scrub removes every secret that may have been echoed back.
func scrub(s string, secrets []string) string {
	for _, k := range secrets {
		if k != "" {
			s = strings.ReplaceAll(s, k, "[redacted]")
		}
	}
	return s
}

func idemKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Client) runPath(id string) (string, error) {
	if !runIDRe.MatchString(id) {
		return "", fmt.Errorf("%q is not a run id", id)
	}
	return "/api/workspaces/" + url.PathEscape(c.WorkspaceID) + "/hosted-runs/" + id, nil
}

// Start asks the server for a new run that accepts publicKey.
func (c *Client) Start(ctx context.Context, o StartOptions, publicKey string) (Run, error) {
	body := map[string]any{"ssh_public_key": publicKey, "name": o.Name, "prompt": o.Prompt}
	if o.FromSnapshot != "" {
		body["from_snapshot"] = o.FromSnapshot
	}
	if o.LLMProvider != "" || o.LLMModel != "" || o.LLMKey != "" {
		body["llm_provider"] = o.LLMProvider
		body["llm_model"] = o.LLMModel
		body["llm_api_key"] = o.LLMKey
	}
	var run Run
	err := c.do(ctx, http.MethodPost, "/api/workspaces/"+url.PathEscape(c.WorkspaceID)+"/hosted-runs", body,
		http.Header{"Idempotency-Key": {idemKey()}}, &run, o.LLMKey)
	return run, err
}

// List returns the workspace's runs.
func (c *Client) List(ctx context.Context) ([]Run, error) {
	var out struct {
		Runs []Run `json:"runs"`
	}
	err := c.do(ctx, http.MethodGet, "/api/workspaces/"+url.PathEscape(c.WorkspaceID)+"/hosted-runs", nil, nil, &out)
	return out.Runs, err
}

// Get returns one run.
func (c *Client) Get(ctx context.Context, id string) (Run, error) {
	p, err := c.runPath(id)
	if err != nil {
		return Run{}, err
	}
	var run Run
	err = c.do(ctx, http.MethodGet, p, nil, nil, &run)
	return run, err
}

// Stop ends a run.
func (c *Client) Stop(ctx context.Context, id string) (Run, error) {
	p, err := c.runPath(id)
	if err != nil {
		return Run{}, err
	}
	var run Run
	err = c.do(ctx, http.MethodDelete, p, nil, nil, &run)
	return run, err
}

// KeyPair is a throwaway ed25519 key for one run.
type KeyPair struct {
	Private []byte // OpenSSH PEM
	Public  string // one authorized_keys line
}

// NewKeyPair makes a fresh key. The private half never leaves this machine.
func NewKeyPair() (KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "rysh-hosted")
	if err != nil {
		return KeyPair{}, err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{
		Private: pem.EncodeToMemory(block),
		Public:  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))),
	}, nil
}

// Alias is the ssh host alias `##remote add` uses for a run.
func Alias(id string) string { return "hosted-" + id }

// Paths are the files kept for one run.
type Paths struct {
	Dir        string
	Key        string
	KnownHosts string
	SSHConfig  string
	Meta       string
}

// Store keeps one private directory per run.
type Store struct{ Dir string }

func (s Store) paths(id string) (Paths, error) {
	if !runIDRe.MatchString(id) {
		return Paths{}, fmt.Errorf("%q is not a run id", id)
	}
	if s.Dir == "" {
		return Paths{}, errors.New("no state directory")
	}
	abs, err := filepath.Abs(s.Dir)
	if err != nil {
		return Paths{}, err
	}
	d := filepath.Join(abs, id)
	return Paths{Dir: d, Key: filepath.Join(d, "key"), KnownHosts: filepath.Join(d, "known_hosts"),
		SSHConfig: filepath.Join(d, "ssh_config"), Meta: filepath.Join(d, "run.json")}, nil
}

func checkRun(r Run) error {
	switch {
	case !runIDRe.MatchString(r.ID):
		return fmt.Errorf("the server sent an unusable run id")
	case !hostRe.MatchString(r.Host):
		return fmt.Errorf("the server sent an unusable host name")
	case r.Port < 1 || r.Port > 65535:
		return fmt.Errorf("the server sent an unusable port")
	case !hostKeyRe.MatchString(r.HostKey):
		return fmt.Errorf("the server sent no usable host key; refusing to connect without pinning one")
	}
	return nil
}

// Save writes the private key, the pinned host key, an ssh_config for
// `##remote add --ssh-config`, and a run.json without any secret.
func (s Store) Save(r Run, kp KeyPair) (Paths, error) { return s.SaveAs(r, kp, SSHUser) }

// sshUserRe is a login name ssh can never read as an option.
var sshUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// SaveAs is Save with the sandbox's login user (rysh Sandboxes VMs log in as
// "user"; the container runner as rysh).
func (s Store) SaveAs(r Run, kp KeyPair, user string) (Paths, error) {
	if !sshUserRe.MatchString(user) {
		return Paths{}, fmt.Errorf("%q is not a login name", user)
	}
	if err := checkRun(r); err != nil {
		return Paths{}, err
	}
	p, err := s.paths(r.ID)
	if err != nil {
		return Paths{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p.Dir), 0o700); err != nil {
		return Paths{}, err
	}
	if err := os.Mkdir(p.Dir, 0o700); err != nil && !os.IsExist(err) {
		return Paths{}, err
	}
	if err := os.Chmod(p.Dir, 0o700); err != nil {
		return Paths{}, err
	}
	fields := strings.Fields(r.HostKey)
	known := fmt.Sprintf("[%s]:%d %s %s\n", r.Host, r.Port, fields[0], fields[1])
	cfg := fmt.Sprintf("Host %s\n    HostName %s\n    Port %d\n    User %s\n    IdentityFile %s\n    IdentitiesOnly yes\n    UserKnownHostsFile %s\n    StrictHostKeyChecking yes\n",
		Alias(r.ID), r.Host, r.Port, user, p.Key, p.KnownHosts)
	meta, _ := json.MarshalIndent(r, "", "  ")
	for _, f := range []struct {
		path string
		data []byte
	}{{p.Key, kp.Private}, {p.KnownHosts, []byte(known)}, {p.SSHConfig, []byte(cfg)}, {p.Meta, append(meta, '\n')}} {
		if err := writePrivate(f.path, f.data); err != nil {
			return Paths{}, err
		}
	}
	return p, nil
}

func writePrivate(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Load reads a saved run.
func (s Store) Load(id string) (Run, Paths, error) {
	p, err := s.paths(id)
	if err != nil {
		return Run{}, Paths{}, err
	}
	b, err := os.ReadFile(p.Meta)
	if err != nil {
		return Run{}, Paths{}, fmt.Errorf("no saved run %s on this machine", id)
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return Run{}, Paths{}, err
	}
	return r, p, nil
}

// List returns every saved run.
func (s Store) List() ([]Run, error) {
	ents, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Run
	for _, e := range ents {
		if !e.IsDir() || !runIDRe.MatchString(e.Name()) {
			continue
		}
		if r, _, err := s.Load(e.Name()); err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// Remove deletes a run's directory, including the private key.
func (s Store) Remove(id string) error {
	p, err := s.paths(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(p.Dir)
}

// Fork asks the server for n forks of run id, each accepting publicKey. The
// forks start from the run's current home; the model key is sent fresh (a
// fork never inherits the parent's) and in the body only.
func (c *Client) Fork(ctx context.Context, id string, n int, o StartOptions, publicKey string) ([]Run, error) {
	p, err := c.runPath(id)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"count": n, "ssh_public_key": publicKey}
	if o.LLMProvider != "" || o.LLMModel != "" || o.LLMKey != "" {
		body["llm_provider"] = o.LLMProvider
		body["llm_model"] = o.LLMModel
		body["llm_api_key"] = o.LLMKey
	}
	var out struct {
		Runs []Run `json:"runs"`
	}
	err = c.do(ctx, http.MethodPost, p+"/fork", body, http.Header{"Idempotency-Key": {idemKey()}}, &out, o.LLMKey)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil, &APIError{ae.Status, "fork is not enabled on this server, or that run does not exist or was not started by you"}
	}
	return out.Runs, err
}
