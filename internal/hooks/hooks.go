// SPDX-License-Identifier: Apache-2.0

// Package hooks runs a workspace's own scripts on agent lifecycle events
// (gap G8, out/gap09-design.md).
//
// The configuration is .rysh/hooks.yaml:
//
//	claude_bridge: true          # claude panes report their events (opt-in)
//	checkpoints: auto            # auto (default) | off — per-turn file checkpoints
//	hooks:
//	  pre-tool:     [{ run: ./scripts/check.sh, match: "Bash|Edit", timeout: 10s }]
//	  post-tool:    [{ run: ./scripts/fmt.sh, match: "Edit|Write" }]
//	  on-needs-you: [{ run: ./scripts/notify.sh }]
//	  on-finish:    [{ run: ./scripts/lint.sh, timeout: 60s }]
//
// `run` is a bash command line, run in the workspace directory with the event
// as one line of JSON on stdin and an allow-listed environment (hookEnviron). `match` is a regexp on the tool name (pre-tool
// and post-tool; empty matches every tool). `timeout` defaults to 30s and may
// not exceed 10m.
//
// Only pre-tool gates, and it fails CLOSED: a non-zero exit, a timeout, a
// script that cannot start, or a hooks.yaml that does not parse blocks the
// tool call. The other events notify: a failure is reported, never blocks.
// A file that does not parse is treated like a broken policy.yaml (design
// 013 §1): an unknown posture, not "no hooks".
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the configuration file inside the workspace's .rysh directory.
const FileName = "hooks.yaml"

const (
	// DefaultTimeout applies when a hook names none.
	DefaultTimeout = 30 * time.Second
	// MaxTimeout is the longest a hook may ask for.
	MaxTimeout = 10 * time.Minute
	// OutputLimit caps what is kept of one hook's output.
	OutputLimit = 64 << 10
)

// Event is a lifecycle point.
type Event string

const (
	PreTool  Event = "pre-tool"
	PostTool Event = "post-tool"
	NeedsYou Event = "on-needs-you"
	Finish   Event = "on-finish"
)

// Events lists every event, in lifecycle order.
var Events = []Event{PreTool, PostTool, NeedsYou, Finish}

// Gates reports whether a failing hook on ev blocks what it guards.
func (ev Event) Gates() bool { return ev == PreTool }

func validEvent(s string) bool {
	for _, e := range Events {
		if string(e) == s {
			return true
		}
	}
	return false
}

// Hook is one configured script.
type Hook struct {
	Run     string `yaml:"run"`
	Match   string `yaml:"match"`
	Timeout string `yaml:"timeout"`

	timeout time.Duration
	match   *regexp.Regexp
}

// Matches reports whether the hook applies to a tool name.
func (h Hook) Matches(tool string) bool { return h.match == nil || h.match.MatchString(tool) }

// TimeoutOrDefault is the hook's effective timeout.
func (h Hook) TimeoutOrDefault() time.Duration { return h.timeout }

type file struct {
	ClaudeBridge bool              `yaml:"claude_bridge"`
	Checkpoints  string            `yaml:"checkpoints"`
	Hooks        map[string][]Hook `yaml:"hooks"`
}

// Config is a loaded hooks.yaml.
type Config struct {
	// Loaded: a file was found and parsed.
	Loaded bool
	// Err: a file was found but cannot be used. Pre-tool then blocks.
	Err  error
	Path string
	// ClaudeBridge: claude panes launched by rysh report their events.
	ClaudeBridge bool
	// AutoCheckpoints: take a file checkpoint per agent turn (default on).
	AutoCheckpoints bool

	hooks map[Event][]Hook
}

// Empty is the configuration of a workspace without a hooks.yaml.
func Empty() *Config { return &Config{AutoCheckpoints: true, hooks: map[Event][]Hook{}} }

// Load reads <ryshDir>/hooks.yaml. A missing file is Empty(); an unreadable
// or invalid one is Empty() with Err set.
func Load(ryshDir string) *Config {
	path := filepath.Join(ryshDir, FileName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Empty()
	}
	c, perr := parse(data)
	if err != nil {
		perr = err
	}
	if perr != nil {
		c = Empty()
		c.Err = fmt.Errorf("%s: %w", path, perr)
	}
	c.Path = path
	return c
}

func parse(data []byte) (*Config, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) { // an empty file is an empty config
		return nil, err
	}
	c := Empty()
	c.Loaded = true
	c.ClaudeBridge = f.ClaudeBridge
	switch strings.TrimSpace(f.Checkpoints) {
	case "", "auto":
	case "off":
		c.AutoCheckpoints = false
	default:
		return nil, fmt.Errorf("checkpoints: %q (want auto or off)", f.Checkpoints)
	}
	for name, list := range f.Hooks {
		if !validEvent(name) {
			return nil, fmt.Errorf("hooks: unknown event %q (want one of %v)", name, Events)
		}
		for i, h := range list {
			where := fmt.Sprintf("hooks.%s[%d]", name, i)
			if strings.TrimSpace(h.Run) == "" {
				return nil, fmt.Errorf("%s: run is empty", where)
			}
			h.timeout = DefaultTimeout
			if s := strings.TrimSpace(h.Timeout); s != "" {
				d, err := time.ParseDuration(s)
				if err != nil || d <= 0 {
					return nil, fmt.Errorf("%s: timeout %q", where, s)
				}
				if d > MaxTimeout {
					return nil, fmt.Errorf("%s: timeout %s exceeds %s", where, d, MaxTimeout)
				}
				h.timeout = d
			}
			if h.Match != "" {
				re, err := regexp.Compile(h.Match)
				if err != nil {
					return nil, fmt.Errorf("%s: match: %w", where, err)
				}
				h.match = re
			}
			c.hooks[Event(name)] = append(c.hooks[Event(name)], h)
		}
	}
	return c, nil
}

// For returns the hooks configured for ev.
func (c *Config) For(ev Event) []Hook {
	if c == nil {
		return nil
	}
	return c.hooks[ev]
}

// Payload is what a hook receives on stdin.
type Payload struct {
	Event   Event           `json:"event"`
	PaneID  string          `json:"pane_id,omitempty"`
	Program string          `json:"program,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Cwd     string          `json:"cwd,omitempty"`
	Reason  string          `json:"reason,omitempty"`
}

// Env is where and with what environment hooks run.
type Env struct {
	// Dir is the working directory (the workspace root).
	Dir string
	// Vars are added to the allow-listed environment (RYSH_PANE and the like).
	Vars []string
}

// allowedEnv names the variables a hook inherits from the process that runs
// it. Nothing else passes: the daemon's and the pane's environment carry
// credential names (rr-msg-193), and a hook is a workspace script. Every LC_*
// passes too.
var allowedEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"LANG": true, "TMPDIR": true, "TZ": true, "TERM": true,
}

// hookEnviron is the allow-listed part of environ.
func hookEnviron(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && (allowedEnv[name] || strings.HasPrefix(name, "LC_")) {
			out = append(out, kv)
		}
	}
	return out
}

// Result is one hook's outcome.
type Result struct {
	Run      string
	ExitCode int
	TimedOut bool
	Output   string
	Err      error
}

// Run runs every hook for ev that applies to p.Tool, in order, and reports
// each outcome. For a gating event it stops at the first failure: the call is
// blocked, and later hooks would only add latency to a decided answer.
func (c *Config) Run(ctx context.Context, ev Event, p Payload, env Env) []Result {
	p.Event = ev
	var out []Result
	for _, h := range c.For(ev) {
		if (ev == PreTool || ev == PostTool) && !h.Matches(p.Tool) {
			continue
		}
		r := runOne(ctx, h, p, env)
		out = append(out, r)
		if ev.Gates() && r.Err != nil {
			break
		}
	}
	return out
}

// Gate runs ev's hooks and reports whether the guarded action is blocked, and
// why. Only a gating event can block.
func (c *Config) Gate(ctx context.Context, ev Event, p Payload, env Env) (bool, string) {
	if !ev.Gates() {
		c.Run(ctx, ev, p, env)
		return false, ""
	}
	if c != nil && c.Err != nil {
		return true, fmt.Sprintf("hooks: %v — every %s call is blocked until it is fixed (##hooks reload)", c.Err, ev)
	}
	for _, r := range c.Run(ctx, ev, p, env) {
		if r.Err != nil {
			reason := fmt.Sprintf("%s hook %q: %v", ev, r.Run, r.Err)
			if o := strings.TrimSpace(r.Output); o != "" {
				reason += ": " + o
			}
			return true, reason
		}
	}
	return false, ""
}

func runOne(ctx context.Context, h Hook, p Payload, env Env) Result {
	r := Result{Run: h.Run}
	timeout := h.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdin, _ := json.Marshal(p)
	cmd := exec.CommandContext(ctx, "bash", "-c", h.Run)
	cmd.Dir = env.Dir
	cmd.Env = append(append(hookEnviron(os.Environ()), env.Vars...), "RYSH_HOOK_EVENT="+string(p.Event))
	killGroupOnCancel(cmd)
	cmd.Stdin = bytes.NewReader(append(stdin, '\n'))
	buf := &capped{limit: OutputLimit}
	cmd.Stdout, cmd.Stderr = buf, buf
	// A hook that leaves a child holding the pipe must not hold the caller.
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	r.Output = buf.String()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		r.TimedOut, r.ExitCode = true, -1
		r.Err = fmt.Errorf("timed out after %s", timeout)
	case err == nil:
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			r.ExitCode = ee.ExitCode()
			r.Err = fmt.Errorf("exited %d", r.ExitCode)
		} else {
			r.ExitCode = -1
			r.Err = fmt.Errorf("failed to run: %w", err)
		}
	}
	return r
}

// capped keeps at most limit bytes and notes that more were dropped.
type capped struct {
	b         bytes.Buffer
	limit     int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
			c.truncated = true
		} else {
			c.b.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

func (c *capped) String() string {
	if c.truncated {
		return c.b.String() + "\n[output truncated]"
	}
	return c.b.String()
}
