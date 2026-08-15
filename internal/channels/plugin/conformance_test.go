// SPDX-License-Identifier: Apache-2.0

package plugin

// Contract conformance (design 002 §6): drive PluginChannelAdapter through
// the full ChannelAdapter surface against the mock plugin process over each
// transport — real JSON-RPC bytes over pipes (stdio) and real NATS messages
// over a listening embedded server (nats).

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// mockManifest builds a manifest whose exec re-launches the test binary as
// the mock plugin (TestHelperPluginProcess).
func mockManifest(t *testing.T, transport string) Manifest {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return Manifest{
		Name:      "mockchan",
		Version:   "0.0.1",
		Transport: transport,
		Exec:      exe + " -test.run=TestHelperPluginProcess",
	}
}

// testOpts returns supervisor options with test-speed timings; extra env
// selects the mock transport/behavior.
func testOpts(extraEnv ...string) SupervisorOptions {
	return SupervisorOptions{
		ExtraEnv:       append([]string{"GO_WANT_PLUGIN_PROCESS=1"}, extraEnv...),
		InitialBackoff: 50 * time.Millisecond,
		MaxBackoff:     300 * time.Millisecond,
		MaxFailures:    5,
		ReadyTimeout:   5 * time.Second,
		StopGrace:      500 * time.Millisecond,
	}
}

// waitFor polls cond until true or the deadline expires.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// recvInboundWith drains the inbound channel until a message satisfying match
// arrives (restarts can interleave duplicate hello messages).
func recvInboundWith(t *testing.T, ch <-chan channels.InboundMessage, d time.Duration, what string, match func(channels.InboundMessage) bool) channels.InboundMessage {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case m := <-ch:
			if match(m) {
				return m
			}
		case <-deadline:
			t.Fatalf("timed out waiting for inbound: %s", what)
		}
	}
}

// startNATSServer boots a LISTENING embedded nats-server (the mock plugin is
// a separate process, so DontListen won't do — mirrors pairing_test's
// embedded-server pattern otherwise) and returns a connected core-side client
// plus the client URL for the plugin's env.
func startNATSServer(t *testing.T) (*nats.Conn, string) {
	t.Helper()
	opts := &natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1, // random free port
		NoLog:  true,
		NoSigs: true,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Skipf("embedded NATS unavailable: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Skip("nats server not ready")
	}
	t.Cleanup(ns.Shutdown)

	url := ns.ClientURL()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect embedded nats: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc, url
}

// runConformance exercises all seven ChannelAdapter methods against the mock
// plugin over the adapter's configured transport.
func runConformance(t *testing.T, a *PluginChannelAdapter) {
	t.Helper()
	ctx := context.Background()

	// Type() — local, no round-trip.
	if got := a.Type(); got != "mockchan" {
		t.Fatalf("Type() = %q, want mockchan", got)
	}

	// Status() before Start: the cached default, non-blocking.
	if st := a.Status(); st.Connected || st.Details != "connecting" {
		t.Fatalf("pre-start Status() = %+v, want disconnected/connecting", st)
	}

	// Start: spawn + ready handshake + start request.
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	// InboundCh: the plugin's inbound notification decodes onto the Go channel
	// with all fields intact.
	hello := recvInboundWith(t, a.InboundCh(), 5*time.Second, "hello-from-plugin",
		func(m channels.InboundMessage) bool { return m.Content == "hello-from-plugin" })
	if hello.SenderID != "mock-sender" || hello.SenderName != "Mock" ||
		hello.ThreadID != "t-1" || hello.Metadata["k"] != "v" {
		t.Fatalf("inbound fields corrupted in transit: %+v", hello)
	}

	// Status(): cached, flips connected via the start path / plugin's push.
	waitFor(t, 5*time.Second, "connected status", func() bool { return a.Status().Connected })

	// Send: normal message round-trips (the mock echoes it back as inbound).
	if err := a.Send(ctx, channels.OutboundMessage{RecipientID: "r-1", ThreadID: "t-1", Content: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	echo := recvInboundWith(t, a.InboundCh(), 5*time.Second, "echo of send",
		func(m channels.InboundMessage) bool { return strings.HasPrefix(m.Content, "echo:") })
	if echo.Content != "echo::hi" || echo.ThreadID != "t-1" {
		t.Fatalf("send encoding corrupted: %+v", echo)
	}

	// Send with Kind=="step" passes Kind through verbatim.
	if err := a.Send(ctx, channels.OutboundMessage{RecipientID: "r-1", Content: "building", Kind: channels.OutboundKindStep}); err != nil {
		t.Fatalf("Send step: %v", err)
	}
	step := recvInboundWith(t, a.InboundCh(), 5*time.Second, "echo of step send",
		func(m channels.InboundMessage) bool { return strings.HasPrefix(m.Content, "echo:step:") })
	if step.Content != "echo:step:building" {
		t.Fatalf("step Kind not passed through: %+v", step)
	}

	// SetReplyMode propagates as a control notification.
	a.SetReplyMode("mentions")
	recvInboundWith(t, a.InboundCh(), 5*time.Second, "replymode echo",
		func(m channels.InboundMessage) bool { return m.Content == "replymode:mentions" })

	// The status *request* wire path (the adapter itself serves Status() from
	// cache; the supervisor/dashboard may issue real requests).
	tr, err := a.sup.transport()
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	var st msg.ChannelStatus
	if err := tr.request(ctx, "status", nil, &st); err != nil {
		t.Fatalf("status request: %v", err)
	}
	if !st.Connected || st.Details != "mock status" {
		t.Fatalf("status reply = %+v", st)
	}

	// Stop: clean shutdown; status reflects it and stays down.
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := a.Status(); st.Connected {
		t.Fatalf("post-stop Status() still connected: %+v", st)
	}
}

func TestConformanceStdio(t *testing.T) {
	m := mockManifest(t, TransportStdio)
	a, err := NewPluginChannelAdapter(m, msg.ChannelConfig{Enabled: true}, testOpts("MOCK_TRANSPORT=stdio"))
	if err != nil {
		t.Fatalf("NewPluginChannelAdapter: %v", err)
	}
	runConformance(t, a)
}

func TestConformanceNATS(t *testing.T) {
	nc, url := startNATSServer(t)
	m := mockManifest(t, TransportNATS)
	opts := testOpts("MOCK_TRANSPORT=nats")
	opts.NATSConn = nc
	opts.NATSURL = url
	a, err := NewPluginChannelAdapter(m, msg.ChannelConfig{Enabled: true}, opts)
	if err != nil {
		t.Fatalf("NewPluginChannelAdapter: %v", err)
	}
	runConformance(t, a)
}

// TestNATSRequiresConn: a nats-transport plugin without a bus handle is
// rejected at construction, not at first use.
func TestNATSRequiresConn(t *testing.T) {
	m := mockManifest(t, TransportNATS)
	if _, err := NewPluginChannelAdapter(m, msg.ChannelConfig{}, testOpts()); err == nil {
		t.Fatal("expected error constructing nats-transport adapter without a bus connection")
	}
}
