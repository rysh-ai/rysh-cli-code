package plugin

// PluginSupervisor — owns the plugin PROCESS (design 002 §4.5). v1 keeps one
// process per running channel for isolation. Responsibilities: spawn the
// manifest's exec (stdio: pipes; nats: env with the bus URL), wait for the
// `ready` handshake before Start replies, monitor process exit, crash-restart
// with exponential backoff (2s → 60s, the same discipline as
// SlackAdapter.connectionLoop), circuit-break after N consecutive failed
// restart attempts, and clean stop (stop request → brief wait → SIGTERM →
// SIGKILL).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Supervisor defaults (design 002 §4.5).
const (
	defaultInitialBackoff = 2 * time.Second
	defaultMaxBackoff     = 60 * time.Second
	defaultMaxFailures    = 5
	defaultReadyTimeout   = 10 * time.Second
	defaultStopGrace      = 2 * time.Second
)

// SupervisorOptions configures process spawning and the restart discipline.
// Zero values take the documented defaults, so tests can shrink timings.
type SupervisorOptions struct {
	// Dir is the plugin's install directory (.rysh/channels/{name}): a
	// relative exec resolves against it and it becomes the process's cwd.
	Dir string
	// TrustFile lists trusted signing keys (DefaultTrustFile when empty).
	// When Dir is set, spawn refuses a plugin whose signature fails
	// verification (SigInvalid); unsigned/unknown-key plugins spawn as
	// before. Dir=="" skips the check (ad-hoc/test adapters have no package
	// directory to verify).
	TrustFile string
	// ExtraEnv is appended to the child's environment.
	ExtraEnv []string
	// NATSConn is the core-side bus handle (required for transport=nats).
	NATSConn *nats.Conn
	// NATSURL is the loopback bus URL handed to a nats-transport plugin via
	// RYSH_PLUGIN_NATS_URL at spawn.
	NATSURL string

	InitialBackoff time.Duration // crash-restart initial backoff (default 2s)
	MaxBackoff     time.Duration // crash-restart backoff cap (default 60s)
	MaxFailures    int           // consecutive failed restarts before circuit-break (default 5)
	ReadyTimeout   time.Duration // max wait for the ready handshake (default 10s)
	StopGrace      time.Duration // per-stage wait during clean stop (default 2s)
}

func (o SupervisorOptions) withDefaults() SupervisorOptions {
	if o.InitialBackoff <= 0 {
		o.InitialBackoff = defaultInitialBackoff
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = defaultMaxBackoff
	}
	if o.MaxFailures <= 0 {
		o.MaxFailures = defaultMaxFailures
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = defaultReadyTimeout
	}
	if o.StopGrace <= 0 {
		o.StopGrace = defaultStopGrace
	}
	return o
}

// PluginSupervisor supervises one plugin process for one running channel.
type PluginSupervisor struct {
	manifest  Manifest
	opts      SupervisorOptions
	onInbound func(channels.InboundMessage)
	statusFn  func(msg.ChannelStatus)

	mu       sync.Mutex
	cmd      *exec.Cmd
	tr       pluginTransport
	procDone chan struct{} // closed when the current process's Wait returns
	startCfg msg.ChannelConfig
	started  bool // Start succeeded; keep the process alive
	stopping bool
	failures int // consecutive failed restart attempts
	// restarting guards the restart loop so at most ONE runs at a time.
	//
	// spawn registers the exit watcher BEFORE the ready handshake (so a
	// process that dies mid-handshake is still recognised). A failed
	// handshake therefore returns an error to a restart loop that is already
	// running, while the watcher for that same dead process fires and tries to
	// start a SECOND loop. Both then increment `failures` against the same
	// budget, so the circuit broke at an arbitrary count above MaxFailures.
	restarting bool
	// broken latches the circuit-break: once tripped, no watcher may open a
	// new restart loop until the channel is explicitly started again.
	broken bool

	stopCh   chan struct{}
	stopOnce sync.Once
}

// newPluginSupervisor builds a supervisor bound to the adapter's callbacks.
func newPluginSupervisor(m Manifest, opts SupervisorOptions, onInbound func(channels.InboundMessage), statusFn func(msg.ChannelStatus)) *PluginSupervisor {
	return &PluginSupervisor{
		manifest:  m,
		opts:      opts.withDefaults(),
		onInbound: onInbound,
		statusFn:  statusFn,
		stopCh:    make(chan struct{}),
	}
}

// Start spawns the plugin, waits for ready, and issues the `start` request.
func (s *PluginSupervisor) Start(ctx context.Context, cfg msg.ChannelConfig) error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return errors.New("plugin supervisor stopped")
	}
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("plugin %q already started", s.manifest.Name)
	}
	s.startCfg = cfg
	// Mark started before spawning so an immediate post-Start crash is picked
	// up by the restart loop; rolled back on spawn failure.
	s.started = true
	// An explicit Start is the operator's "try again" after a circuit-break —
	// clear the latch and the failure budget, or a previously broken channel
	// could never come back without recreating the adapter.
	s.broken = false
	s.failures = 0
	s.mu.Unlock()

	if err := s.spawnAndStart(ctx); err != nil {
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		return err
	}
	return nil
}

// setReplyMode keeps the stored start config current so a restarted plugin
// receives the latest mode in its `start` payload.
func (s *PluginSupervisor) setReplyMode(mode string) {
	s.mu.Lock()
	s.startCfg.ReplyMode = mode
	s.mu.Unlock()
}

// transport returns the current transport, or an error while the plugin
// process is down (crashed / restarting / stopped).
func (s *PluginSupervisor) transport() (pluginTransport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tr == nil {
		return nil, errors.New("plugin process not running")
	}
	return s.tr, nil
}

// spawnAndStart spawns a process (ready handshake included) and issues the
// `start` request with the stored channel config.
func (s *PluginSupervisor) spawnAndStart(ctx context.Context) error {
	if err := s.spawn(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	tr, cmd, cfg := s.tr, s.cmd, s.startCfg
	s.mu.Unlock()
	if tr == nil || cmd == nil {
		return errors.New("plugin transport unavailable after spawn")
	}

	rctx, cancel := context.WithTimeout(ctx, pluginRPCTimeout)
	defer cancel()
	if err := tr.request(rctx, "start", cfg, nil); err != nil {
		s.teardownProcess(cmd)
		return fmt.Errorf("plugin %q start: %w", s.manifest.Name, err)
	}

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	s.statusFn(msg.ChannelStatus{
		Type:      s.manifest.Name,
		Connected: true,
		Details:   fmt.Sprintf("plugin running (pid %d, transport %s)", pid, s.manifest.Transport),
	})
	slog.Info("plugin: channel plugin started",
		"plugin", s.manifest.Name, "pid", pid, "transport", s.manifest.Transport)
	return nil
}

// spawn launches the exec from the manifest and waits for the ready
// handshake (stdio: `ready` notification; nats: publish on `.ready`).
func (s *PluginSupervisor) spawn(ctx context.Context) error {
	// Signature enforcement (dev tier): a signed-but-tampered package must
	// never execute. Re-checked on every spawn, so a binary swapped under a
	// running plugin is refused at the next restart.
	if s.opts.Dir != "" {
		if res := VerifyPlugin(s.opts.Dir, s.manifest, s.opts.TrustFile); res.Status == SigInvalid {
			slog.Error("plugin: refusing to spawn, signature verification failed",
				"plugin", s.manifest.Name, "dir", s.opts.Dir, "key_id", res.KeyID, "reason", res.Reason)
			return fmt.Errorf("plugin %q: signature verification failed: %s", s.manifest.Name, res.Reason)
		}
	}

	fields := strings.Fields(s.manifest.Exec)
	if len(fields) == 0 {
		return fmt.Errorf("plugin %q: empty exec", s.manifest.Name)
	}
	bin := fields[0]
	if !filepath.IsAbs(bin) && s.opts.Dir != "" {
		if cand := filepath.Join(s.opts.Dir, bin); fileExists(cand) {
			bin = cand
		}
	}

	readyCh := make(chan struct{})
	var readyOnce sync.Once
	h := transportHandlers{
		onInbound: s.onInbound,
		onStatus:  s.statusFn,
		onReady:   func() { readyOnce.Do(func() { close(readyCh) }) },
	}

	cmd := exec.Command(bin, fields[1:]...)
	cmd.Dir = s.opts.Dir
	env := append(os.Environ(), s.opts.ExtraEnv...)
	env = append(env, "RYSH_CHANNEL_PLUGIN_NAME="+s.manifest.Name)

	var tr pluginTransport
	switch s.manifest.Transport {
	case TransportNATS:
		if s.opts.NATSConn == nil {
			return fmt.Errorf("plugin %q: transport %q requires a session bus connection", s.manifest.Name, TransportNATS)
		}
		env = append(env, "RYSH_PLUGIN_NATS_URL="+s.opts.NATSURL)
		cmd.Env = env
		cmd.Stdout = os.Stderr // a nats plugin's stdout is just logging
		cmd.Stderr = os.Stderr
		// Subscriptions (including .ready) exist before the process starts,
		// so the handshake cannot be missed.
		nt, err := newNATSTransport(s.opts.NATSConn, s.manifest.Name, h)
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			_ = nt.close()
			return fmt.Errorf("plugin %q: spawn %s: %w", s.manifest.Name, bin, err)
		}
		tr = nt

	case TransportStdio:
		cmd.Env = env
		cmd.Stderr = os.Stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("plugin %q: stdin pipe: %w", s.manifest.Name, err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("plugin %q: stdout pipe: %w", s.manifest.Name, err)
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("plugin %q: spawn %s: %w", s.manifest.Name, bin, err)
		}
		tr = newStdioTransport(s.manifest.Name, stdin, stdout, h)

	default:
		return fmt.Errorf("plugin %q: unsupported transport %q", s.manifest.Name, s.manifest.Transport)
	}

	procDone := make(chan struct{})
	go func() {
		err := cmd.Wait()
		close(procDone)
		s.handleExit(cmd, err)
	}()

	// Register BEFORE the ready wait so handleExit recognizes this process
	// (an exit-before-ready still routes through the identity check, and the
	// restart loop is suppressed because we deregister on the failure paths).
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		_ = tr.close()
		killProcess(cmd)
		return errors.New("plugin supervisor stopped")
	}
	s.cmd, s.tr, s.procDone = cmd, tr, procDone
	s.mu.Unlock()

	select {
	case <-readyCh:
		return nil
	case <-procDone:
		s.teardownProcess(cmd)
		return fmt.Errorf("plugin %q: process exited before ready handshake", s.manifest.Name)
	case <-time.After(s.opts.ReadyTimeout):
		s.teardownProcess(cmd)
		return fmt.Errorf("plugin %q: ready handshake timeout after %s", s.manifest.Name, s.opts.ReadyTimeout)
	case <-ctx.Done():
		s.teardownProcess(cmd)
		return ctx.Err()
	case <-s.stopCh:
		s.teardownProcess(cmd)
		return errors.New("plugin supervisor stopped")
	}
}

// teardownProcess deregisters cmd (so handleExit does not treat the kill as a
// crash), closes its transport, and kills the process.
func (s *PluginSupervisor) teardownProcess(cmd *exec.Cmd) {
	s.mu.Lock()
	var tr pluginTransport
	if s.cmd == cmd {
		tr = s.tr
		s.cmd, s.tr, s.procDone = nil, nil, nil
	}
	s.mu.Unlock()
	if tr != nil {
		_ = tr.close()
	}
	killProcess(cmd)
}

// handleExit runs when the current process's Wait returns. Unexpected exits
// flip status to restarting and enter the backoff-restart loop.
func (s *PluginSupervisor) handleExit(cmd *exec.Cmd, exitErr error) {
	s.mu.Lock()
	if s.cmd != cmd {
		// Already torn down (clean stop, start-failure, or a superseded
		// process) — nothing to do.
		s.mu.Unlock()
		return
	}
	tr := s.tr
	s.cmd, s.tr, s.procDone = nil, nil, nil
	stopping, started := s.stopping, s.started
	s.mu.Unlock()

	if tr != nil {
		_ = tr.close()
	}
	if stopping || !started {
		return
	}

	cause := "exited"
	if exitErr != nil {
		cause = exitErr.Error()
	}
	if !s.beginRestart() {
		// Another restart loop already owns the budget, or the circuit is
		// open. Either way this exit must not start a competing loop.
		slog.Debug("plugin: exit observed while a restart is already in progress",
			"plugin", s.manifest.Name, "err", cause)
		return
	}
	defer s.endRestart()

	slog.Warn("plugin: process exited unexpectedly, entering restart loop",
		"plugin", s.manifest.Name, "err", cause)
	s.restartLoop(cause)
}

// restartLoop respawns the plugin with capped exponential backoff. After
// MaxFailures consecutive failed attempts it circuit-breaks: stops
// restarting and surfaces the reason in the cached status.
func (s *PluginSupervisor) restartLoop(cause string) {
	backoff := s.opts.InitialBackoff
	for {
		s.statusFn(msg.ChannelStatus{
			Type:      s.manifest.Name,
			Connected: false,
			Error:     "plugin restarting",
			Details:   fmt.Sprintf("process %s; restarting in %s", cause, backoff),
		})

		select {
		case <-time.After(backoff):
		case <-s.stopCh:
			return
		}
		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		err := s.spawnAndStart(context.Background())
		if err == nil {
			s.mu.Lock()
			s.failures = 0
			s.mu.Unlock()
			slog.Info("plugin: restarted after crash", "plugin", s.manifest.Name)
			return // the new process's Wait goroutine monitors from here
		}

		s.mu.Lock()
		s.failures++
		failures := s.failures
		s.mu.Unlock()
		cause = err.Error()
		slog.Warn("plugin: restart attempt failed",
			"plugin", s.manifest.Name, "failures", failures, "err", err)

		if failures >= s.opts.MaxFailures {
			s.mu.Lock()
			s.broken = true
			s.mu.Unlock()
			s.statusFn(msg.ChannelStatus{
				Type:      s.manifest.Name,
				Connected: false,
				Error: fmt.Sprintf("plugin circuit-break: %d consecutive restart failures (last: %v)",
					failures, err),
				Details: "not restarting — fix or reinstall the plugin, then restart the channel",
			})
			slog.Error("plugin: circuit-break, giving up on restarts",
				"plugin", s.manifest.Name, "failures", failures)
			return
		}

		backoff *= 2
		if backoff > s.opts.MaxBackoff {
			backoff = s.opts.MaxBackoff
		}
	}
}

// Stop performs the clean-stop sequence: `stop` request → brief wait →
// SIGTERM → SIGKILL. The supervisor is terminal after Stop.
func (s *PluginSupervisor) Stop() error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	tr, cmd, procDone := s.tr, s.cmd, s.procDone
	s.cmd, s.tr, s.procDone = nil, nil, nil
	s.mu.Unlock()
	s.stopOnce.Do(func() { close(s.stopCh) })

	if tr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.StopGrace)
		if err := tr.request(ctx, "stop", nil, nil); err != nil {
			slog.Debug("plugin: stop request failed (proceeding to signal)",
				"plugin", s.manifest.Name, "err", err)
		}
		cancel()
		_ = tr.close()
	}

	if cmd != nil && cmd.Process != nil {
		if !waitClosed(procDone, s.opts.StopGrace) {
			slog.Debug("plugin: sending SIGTERM", "plugin", s.manifest.Name, "pid", cmd.Process.Pid)
			_ = cmd.Process.Signal(syscall.SIGTERM)
			if !waitClosed(procDone, s.opts.StopGrace) {
				slog.Warn("plugin: SIGKILL after stop grace", "plugin", s.manifest.Name, "pid", cmd.Process.Pid)
				_ = cmd.Process.Kill()
				waitClosed(procDone, s.opts.StopGrace)
			}
		}
	}
	slog.Info("plugin: supervisor stopped", "plugin", s.manifest.Name)
	return nil
}

// beginRestart claims the single restart slot. It returns false when the
// supervisor is stopping, the circuit is already open, or another loop is
// running — in which case the caller must NOT restart.
func (s *PluginSupervisor) beginRestart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping || s.broken || s.restarting {
		return false
	}
	s.restarting = true
	return true
}

// endRestart releases the restart slot.
func (s *PluginSupervisor) endRestart() {
	s.mu.Lock()
	s.restarting = false
	s.mu.Unlock()
}

// failuresCount reports the consecutive-failed-restart counter (tests,
// dashboards).
func (s *PluginSupervisor) failuresCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures
}

// waitClosed waits up to d for ch to close. A nil channel returns true (no
// process to wait on).
func waitClosed(ch chan struct{}, d time.Duration) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func killProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
