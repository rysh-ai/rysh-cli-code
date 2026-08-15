// SPDX-License-Identifier: Apache-2.0

// Package tunnel publishes a locally-bound rysh web server at a public HTTPS
// URL through ngrok.
//
// The web server binds a port on this machine; a phone or a laptop somewhere
// else cannot reach it. An ngrok tunnel is the door, and it is a door the
// session should be able to open for itself on every restart — the alternative
// is a hand-run `ngrok http` (or a LaunchAgent) that nobody remembers to
// restart, whose URL nothing in the session knows.
//
// There are three ways in, tried in this order:
//
//	adopt   an agent is already running and already forwards our port —
//	        use its URL and touch nothing
//	create  an agent is running but not for our port — ask it, over its local
//	        API, to add one
//	spawn   no agent is running — start `ngrok http <port>` ourselves
//
// The order is not a preference, it is the free plan: ONE agent session per
// account. A second `ngrok http` exits immediately with ERR_NGROK_108 and, if
// something respins it, rate-limits the account. So an agent that is already up
// is always used rather than competed with — and Stop() only ever tears down
// what this process actually opened.
package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultAPIBase is the ngrok agent's local API — the same one that serves the
// inspector at http://127.0.0.1:4040.
const DefaultAPIBase = "http://127.0.0.1:4040"

// DefaultBinary is the ngrok executable, resolved through PATH when it is not
// an absolute path.
const DefaultBinary = "ngrok"

// defaultStartTimeout bounds how long Start waits for a spawned agent to report
// a public URL. A cold ngrok dial is a couple of seconds; anything past this is
// a failure worth reporting rather than waiting on.
const defaultStartTimeout = 25 * time.Second

// Origin records how a tunnel came to exist, which is exactly what decides
// whether Stop may take it down.
type Origin string

const (
	// OriginAdopted: an agent was already forwarding this port. Not ours to stop.
	OriginAdopted Origin = "adopted"
	// OriginCreated: a running agent created it at our request, over its API.
	OriginCreated Origin = "created"
	// OriginSpawned: we started the agent process ourselves.
	OriginSpawned Origin = "spawned"
)

// Options configures one tunnel.
type Options struct {
	// Port is the local TCP port to publish. Required.
	Port int
	// Domain is a reserved ngrok domain (e.g. "rysh-web.ngrok.app"). Empty
	// takes whatever ephemeral URL ngrok hands out — which changes on every
	// restart, so a session meant to be reachable at a stable address wants one.
	Domain string
	// Binary is the ngrok executable; empty means DefaultBinary.
	Binary string
	// ConfigFile is an ngrok config to pass through (--config).
	ConfigFile string
	// Authtoken authenticates a spawned agent (NGROK_AUTHTOKEN). Empty leaves
	// the agent to find its own in ~/.../ngrok.yml, which is the usual setup.
	Authtoken string
	// APIBase overrides the agent API base URL; empty means DefaultAPIBase.
	APIBase string
	// Name is the tunnel name used with the agent API. Empty derives one from
	// the port ("rysh-web-<port>").
	Name string
	// LogPath is where a spawned agent's log is written. Empty discards it —
	// but a log is what turns "the tunnel did not come up" into a reason, so
	// callers should set one.
	LogPath string
	// Timeout bounds Start. Zero means defaultStartTimeout.
	Timeout time.Duration
	// HTTPClient overrides the client used for the agent API (tests).
	HTTPClient *http.Client
}

// Tunnel is a live public endpoint for a local port.
type Tunnel struct {
	// URL is the public HTTPS address.
	URL string
	// Origin says how it was established, and so what Stop is allowed to do.
	Origin Origin
	// Name is the agent-API tunnel name.
	Name string
	// Port is the local port being published.
	Port int

	apiBase string
	client  *http.Client
	cmd     *exec.Cmd
	logFile *os.File
	stopped bool
}

// tunnelInfo is the subset of the agent API's tunnel object we read.
type tunnelInfo struct {
	Name      string `json:"name"`
	PublicURL string `json:"public_url"`
	Proto     string `json:"proto"`
	Config    struct {
		Addr string `json:"addr"`
	} `json:"config"`
}

// Start establishes a tunnel for opts.Port, adopting or reusing a running agent
// before starting one of its own (see the package comment).
func Start(ctx context.Context, opts Options) (*Tunnel, error) {
	if opts.Port <= 0 {
		return nil, fmt.Errorf("tunnel: port is required")
	}
	apiBase := strings.TrimRight(strings.TrimSpace(opts.APIBase), "/")
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = fmt.Sprintf("rysh-web-%d", opts.Port)
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	t := &Tunnel{Name: name, Port: opts.Port, apiBase: apiBase, client: client}

	// 1. Adopt — an agent already publishes this port.
	if existing, err := t.findTunnel(ctx, opts.Port); err == nil && existing != nil {
		t.URL, t.Origin, t.Name = existing.PublicURL, OriginAdopted, existing.Name
		return t, nil
	} else if err == nil {
		// 2. Create — the agent answered, it just has no tunnel for us. Asking
		// it beats starting a second agent the account is not allowed to have.
		created, cerr := t.createTunnel(ctx, opts)
		if cerr == nil {
			t.URL, t.Origin, t.Name = created.PublicURL, OriginCreated, created.Name
			return t, nil
		}
		// A running agent that refuses the request is worth reporting as-is:
		// spawning a rival agent behind its back would only fail with
		// ERR_NGROK_108 and bury this reason.
		return nil, fmt.Errorf("ngrok agent is running but would not add a tunnel for port %d: %w", opts.Port, cerr)
	}

	// 3. Spawn — no agent is running.
	if err := t.spawn(ctx, opts); err != nil {
		t.releaseProcess()
		return nil, err
	}
	return t, nil
}

// Stop takes down the tunnel — but only what this process opened. An adopted
// tunnel belongs to whoever started it (a LaunchAgent, another session, the
// user's own terminal) and is left alone; stopping it would cut a door this
// process never opened.
func (t *Tunnel) Stop() error {
	if t == nil || t.stopped {
		return nil
	}
	t.stopped = true
	switch t.Origin {
	case OriginCreated:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.apiBase+"/api/tunnels/"+t.Name, nil)
		if err != nil {
			return err
		}
		resp, err := t.client.Do(req)
		if err != nil {
			return fmt.Errorf("close tunnel %s: %w", t.Name, err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("close tunnel %s: agent replied %s", t.Name, resp.Status)
		}
		return nil
	case OriginSpawned:
		return t.releaseProcess()
	default:
		return nil
	}
}

// Adopted reports whether the tunnel was already up when this process asked for
// it — the case where Stop deliberately does nothing.
func (t *Tunnel) Adopted() bool { return t != nil && t.Origin == OriginAdopted }

// releaseProcess ends a spawned agent: SIGINT first so ngrok closes its session
// cleanly (leaving one open on a single-session plan is what blocks the NEXT
// start), then SIGKILL if it will not go.
func (t *Tunnel) releaseProcess() error {
	defer func() {
		if t.logFile != nil {
			_ = t.logFile.Close()
			t.logFile = nil
		}
	}()
	if t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	proc := t.cmd.Process
	_ = proc.Signal(syscall.SIGINT)
	done := make(chan struct{})
	go func() { _, _ = t.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = proc.Kill()
	}
	t.cmd = nil
	return nil
}

// findTunnel asks the agent API for a tunnel already forwarding to port. A nil
// tunnel with a nil error means "the agent answered, and has none".
func (t *Tunnel) findTunnel(ctx context.Context, port int) (*tunnelInfo, error) {
	list, err := t.listTunnels(ctx)
	if err != nil {
		return nil, err
	}
	var match *tunnelInfo
	for i := range list {
		if addrPort(list[i].Config.Addr) != port {
			continue
		}
		// An agent may list the same tunnel twice (http and https). HTTPS is the
		// one worth handing to a browser.
		if match == nil || strings.EqualFold(list[i].Proto, "https") {
			match = &list[i]
		}
	}
	return match, nil
}

// listTunnels reads the agent's tunnel list. An error here means no agent is
// listening (or it is not answering), which is the signal to spawn one.
func (t *Tunnel) listTunnels(ctx context.Context) ([]tunnelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.apiBase+"/api/tunnels", nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ngrok agent API: %s", resp.Status)
	}
	var payload struct {
		Tunnels []tunnelInfo `json:"tunnels"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("ngrok agent API: %w", err)
	}
	return payload.Tunnels, nil
}

// createTunnel asks a RUNNING agent to publish our port, which is how a second
// tunnel is opened on an account allowed only one agent session.
func (t *Tunnel) createTunnel(ctx context.Context, opts Options) (*tunnelInfo, error) {
	body := map[string]any{
		"name":  t.Name,
		"proto": "http",
		"addr":  strconv.Itoa(opts.Port),
	}
	if d := strings.TrimSpace(opts.Domain); d != "" {
		body["domain"] = d
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.apiBase+"/api/tunnels", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", resp.Status, agentError(data))
	}
	var info tunnelInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("decode agent reply: %w", err)
	}
	if info.PublicURL == "" {
		// Some agent versions answer the create with the http twin and register
		// the https one alongside it; ask the list for the better URL.
		if found, ferr := t.findTunnel(ctx, opts.Port); ferr == nil && found != nil {
			return found, nil
		}
		return nil, fmt.Errorf("agent created no public URL")
	}
	return &info, nil
}

// spawn starts an ngrok agent for this port and waits for its public URL.
func (t *Tunnel) spawn(ctx context.Context, opts Options) error {
	bin := strings.TrimSpace(opts.Binary)
	if bin == "" {
		bin = DefaultBinary
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("ngrok not found (%s): install it from https://ngrok.com/download, or set [web] ngrok_binary", bin)
	}
	args := []string{"http", strconv.Itoa(opts.Port), "--log", "stdout", "--log-format", "logfmt"}
	if d := strings.TrimSpace(opts.Domain); d != "" {
		args = append(args, "--domain", d)
	}
	if c := strings.TrimSpace(opts.ConfigFile); c != "" {
		args = append(args, "--config", c)
	}
	cmd := exec.Command(resolved, args...)
	cmd.Env = os.Environ()
	if tok := strings.TrimSpace(opts.Authtoken); tok != "" {
		cmd.Env = append(cmd.Env, "NGROK_AUTHTOKEN="+tok)
	}
	// Its own process group: a SIGINT delivered to the daemon's group (Ctrl-C in
	// a foreground terminal) must not take the tunnel down behind the session's
	// back — Stop owns that decision.
	detachProcessGroup(cmd)
	if lp := strings.TrimSpace(opts.LogPath); lp != "" {
		_ = os.MkdirAll(filepath.Dir(lp), 0o755)
		if f, ferr := os.OpenFile(lp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); ferr == nil {
			t.logFile = f
			cmd.Stdout, cmd.Stderr = f, f
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ngrok: %w", err)
	}
	t.cmd = cmd

	// The agent exiting is a result too — and the fast one, since a refused
	// session (ERR_NGROK_108) is reported in well under a second.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultStartTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case werr := <-exited:
			t.cmd = nil
			return fmt.Errorf("ngrok exited before the tunnel came up (%v)%s", werr, t.logHint(opts.LogPath))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		if found, ferr := t.findTunnel(ctx, opts.Port); ferr == nil && found != nil {
			t.URL, t.Origin = found.PublicURL, OriginSpawned
			if found.Name != "" {
				t.Name = found.Name
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ngrok did not publish port %d within %s%s", opts.Port, timeout, t.logHint(opts.LogPath))
		}
	}
}

// logHint appends the tail of a spawned agent's log, so the caller reports
// ngrok's own reason (a bad authtoken, a taken domain, a used-up session slot)
// instead of a bare timeout.
func (t *Tunnel) logHint(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if n := len(lines); n > 3 {
		lines = lines[n-3:]
	}
	return ": " + strings.Join(lines, " | ")
}

// agentError pulls the human-readable message out of an agent API error body,
// falling back to the raw body.
func agentError(data []byte) string {
	var payload struct {
		Msg     string `json:"msg"`
		Details struct {
			Err string `json:"err"`
		} `json:"details"`
	}
	if err := json.Unmarshal(data, &payload); err == nil {
		switch {
		case payload.Details.Err != "":
			return strings.TrimSpace(payload.Details.Err)
		case payload.Msg != "":
			return strings.TrimSpace(payload.Msg)
		}
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "no detail"
	}
	return s
}

// addrPort extracts the local port from an agent's forwarding address, which it
// spells variously: "23001", "localhost:23001", "http://localhost:23001".
func addrPort(addr string) int {
	s := strings.TrimSpace(addr)
	if s == "" {
		return 0
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	p, err := strconv.Atoi(s)
	if err != nil || p <= 0 || p > 65535 {
		return 0
	}
	return p
}

// ErrNoTunnel reports that nothing is published for a port.
var ErrNoTunnel = errors.New("no tunnel")

// Lookup reports the public URL a running agent already serves for port,
// without establishing anything. Used by status output, where starting a tunnel
// as a side effect of asking about one would be a surprise.
func Lookup(ctx context.Context, apiBase string, port int) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = DefaultAPIBase
	}
	t := &Tunnel{apiBase: base, client: &http.Client{Timeout: 3 * time.Second}}
	found, err := t.findTunnel(ctx, port)
	if err != nil {
		return "", err
	}
	if found == nil {
		return "", ErrNoTunnel
	}
	return found.PublicURL, nil
}
