// SPDX-License-Identifier: Apache-2.0

package plugin

// Lifecycle tests (design 002 §4.5): crash → backoff restart → connected;
// circuit-break after N consecutive failed restarts; clean stop with no
// resurrection.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestSupervisorCrashRestart kills the plugin mid-run and asserts the status
// flips restarting → connected again, and the restarted process serves sends.
func TestSupervisorCrashRestart(t *testing.T) {
	m := mockManifest(t, TransportStdio)
	opts := testOpts("MOCK_TRANSPORT=stdio")
	opts.InitialBackoff = 100 * time.Millisecond

	a, err := NewPluginChannelAdapter(m, msg.ChannelConfig{Enabled: true}, opts)
	if err != nil {
		t.Fatalf("NewPluginChannelAdapter: %v", err)
	}
	defer a.Stop()

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 5*time.Second, "initial connected", func() bool { return a.Status().Connected })

	// Crash the plugin process ("__die__" makes it exit(1) without replying;
	// the in-flight Send errors cleanly rather than hanging).
	sendCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Send(sendCtx, channels.OutboundMessage{Content: "__die__"}); err == nil {
		t.Fatal("Send to a crashing plugin should error")
	}

	// Status flips to restarting…
	waitFor(t, 5*time.Second, "restarting status", func() bool {
		st := a.Status()
		return !st.Connected && st.Error == "plugin restarting"
	})
	// …then back to connected after the backoff respawn.
	waitFor(t, 10*time.Second, "reconnected status", func() bool { return a.Status().Connected })

	// The restarted process is functional end to end.
	if err := a.Send(context.Background(), channels.OutboundMessage{Content: "post-restart", ThreadID: "t-2"}); err != nil {
		t.Fatalf("Send after restart: %v", err)
	}
	recvInboundWith(t, a.InboundCh(), 5*time.Second, "post-restart echo",
		func(im channels.InboundMessage) bool { return im.Content == "echo::post-restart" })
}

// TestSupervisorCrashOnceBehavior exercises the crash-after-ready path: the
// first process dies on its own shortly after start; the marker makes the
// respawn behave normally.
func TestSupervisorCrashOnceBehavior(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crash-once-marker")
	m := mockManifest(t, TransportStdio)
	opts := testOpts("MOCK_TRANSPORT=stdio", "MOCK_BEHAVIOR=crash-once", "MOCK_MARKER="+marker)
	opts.InitialBackoff = 50 * time.Millisecond

	a, err := NewPluginChannelAdapter(m, msg.ChannelConfig{Enabled: true}, opts)
	if err != nil {
		t.Fatalf("NewPluginChannelAdapter: %v", err)
	}
	defer a.Stop()

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 5*time.Second, "initial connected", func() bool { return a.Status().Connected })

	// First process crashes on its own ~150ms in.
	waitFor(t, 10*time.Second, "autonomous crash observed", func() bool { return !a.Status().Connected })

	// Supervisor must land on a healthy second process without any caller
	// involvement (marker file flips the mock to normal behavior).
	waitFor(t, 10*time.Second, "self-healed connected status", func() bool {
		st := a.Status()
		return st.Connected && a.sup.failuresCount() == 0
	})
	if err := a.Send(context.Background(), channels.OutboundMessage{Content: "alive"}); err != nil {
		t.Fatalf("Send after self-heal: %v", err)
	}
}

// TestSupervisorCircuitBreak: after the first crash every respawn exits
// before the ready handshake; the supervisor must stop restarting after
// MaxFailures attempts and surface the circuit-break in Status().
func TestSupervisorCircuitBreak(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "fail-marker")
	m := mockManifest(t, TransportStdio)
	opts := testOpts("MOCK_TRANSPORT=stdio", "MOCK_BEHAVIOR=die-then-fail", "MOCK_MARKER="+marker)
	opts.InitialBackoff = 20 * time.Millisecond
	opts.MaxBackoff = 50 * time.Millisecond
	opts.MaxFailures = 3
	opts.ReadyTimeout = 2 * time.Second

	a, err := NewPluginChannelAdapter(m, msg.ChannelConfig{Enabled: true}, opts)
	if err != nil {
		t.Fatalf("NewPluginChannelAdapter: %v", err)
	}
	defer a.Stop()

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 5*time.Second, "initial connected", func() bool { return a.Status().Connected })

	sendCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = a.Send(sendCtx, channels.OutboundMessage{Content: "__die__"})

	waitFor(t, 30*time.Second, "circuit-break status", func() bool {
		st := a.Status()
		return !st.Connected && strings.Contains(st.Error, "circuit-break")
	})
	if got := a.sup.failuresCount(); got != opts.MaxFailures {
		t.Fatalf("failures = %d, want %d", got, opts.MaxFailures)
	}

	// Circuit is open: no further restarts happen.
	time.Sleep(200 * time.Millisecond)
	if st := a.Status(); st.Connected || !strings.Contains(st.Error, "circuit-break") {
		t.Fatalf("circuit reopened unexpectedly: %+v", st)
	}
}

// TestSupervisorCleanStop: Stop shuts the process down and nothing restarts.
func TestSupervisorCleanStop(t *testing.T) {
	m := mockManifest(t, TransportStdio)
	a, err := NewPluginChannelAdapter(m, msg.ChannelConfig{Enabled: true}, testOpts("MOCK_TRANSPORT=stdio"))
	if err != nil {
		t.Fatalf("NewPluginChannelAdapter: %v", err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 5*time.Second, "connected", func() bool { return a.Status().Connected })

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := a.Status(); st.Connected || st.Details != "stopped" {
		t.Fatalf("post-stop status = %+v", st)
	}

	// No resurrection: give a would-be restart loop time to fire.
	time.Sleep(300 * time.Millisecond)
	if st := a.Status(); st.Connected || st.Error == "plugin restarting" {
		t.Fatalf("plugin restarted after clean stop: %+v", st)
	}
	if _, err := a.sup.transport(); err == nil {
		t.Fatal("transport should be gone after Stop")
	}

	// Stop is idempotent.
	if err := a.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestCircuitLatchBlocksReentry pins the circuit-break latch. The old break was
// a bare `return`: it ended one restart loop but left a later exit free to start
// another, which drove failures past MaxFailures and overwrote the
// circuit-break status with "plugin restarting". The latch must make re-entry
// impossible, and only an explicit Start may clear it.
//
// The implementation is dev's (12ec13f/eb0f46b, which fixed this independently
// and more cleanly via beginRestart/endRestart); these two regression tests are
// this branch's contribution — dev shipped the fix without them.
func TestCircuitLatchBlocksReentry(t *testing.T) {
	s := newPluginSupervisor(mockManifest(t, TransportStdio), testOpts().withDefaults(), nil, func(msg.ChannelStatus) {})

	s.mu.Lock()
	s.started, s.broken, s.failures = true, true, 7
	s.mu.Unlock()

	// beginRestart IS the gate (handleExit calls it before ever entering the
	// loop), so assert the gate rather than calling restartLoop directly —
	// restartLoop is deliberately unguarded and would spawn a real process.
	if s.beginRestart() {
		t.Error("beginRestart granted the restart slot with the circuit open")
	}
	if got := s.failuresCount(); got != 7 {
		t.Errorf("the gate must not touch the failure budget: failures = %d, want 7", got)
	}

	// A process exit while the circuit is open must not re-enter either.
	s.mu.Lock()
	s.cmd = nil
	s.mu.Unlock()
	s.handleExit(nil, nil) // identity check returns early; must not panic or restart
	if got := s.failuresCount(); got != 7 {
		t.Errorf("handleExit re-entered the restart loop: failures = %d, want 7", got)
	}

	// Only an explicit Start reopens it.
	s.mu.Lock()
	s.started = false
	s.mu.Unlock()
	_ = s.Start(context.Background(), msg.ChannelConfig{Enabled: true})
	s.mu.Lock()
	open, failures := s.broken, s.failures
	s.mu.Unlock()
	if open {
		t.Error("Start must reopen the circuit for a fixed/reinstalled plugin")
	}
	if failures != 0 {
		t.Errorf("Start must reset the failure counter, got %d", failures)
	}
	_ = s.Stop()
}

// TestConcurrentRestartLoopsAreSerialised covers the `restarting` guard: the
// crash-before-ready race could start a second loop alongside the first.
func TestConcurrentRestartLoopsAreSerialised(t *testing.T) {
	s := newPluginSupervisor(mockManifest(t, TransportStdio), testOpts().withDefaults(), nil, func(msg.ChannelStatus) {})
	s.mu.Lock()
	s.started, s.restarting = true, true // a loop is already running
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.restartLoop("exit status 1"); close(done) }()
	select {
	case <-done: // must return immediately, not spawn a competing loop
	case <-time.After(2 * time.Second):
		t.Fatal("a second restartLoop did not bail out — the guard is not holding")
	}
	if got := s.failuresCount(); got != 0 {
		t.Errorf("the second loop did work: failures = %d, want 0", got)
	}
}
