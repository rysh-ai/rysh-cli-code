package plugin

// The reference mock plugin used by the conformance/supervisor tests. It is a
// REAL separate process: tests set the manifest exec to the test binary
// itself with `-test.run=TestHelperPluginProcess` (the classic helper-process
// re-exec trick), so the wire protocol is exercised as actual
// newline-delimited JSON-RPC bytes over OS pipes (stdio) or actual NATS
// messages over a listening embedded server (nats).
//
// Behaviors (env MOCK_BEHAVIOR):
//   - ""             normal: ready → serve start/stop/send/status/setReplyMode
//   - "crash-once"   first run (no MOCK_MARKER file yet): serve start, then
//                    exit(1) shortly after — later runs behave normally.
//                    Exercises backoff restart → connected.
//   - "die-then-fail" first run normal (test crashes it via a "__die__"
//                    send); every later run exits(1) BEFORE the ready
//                    handshake. Exercises the restart circuit-break.
//
// A send with Content "__die__" makes the plugin exit(1) without replying.
// Every accepted send is echoed back as an inbound "echo:<kind>:<content>"
// so tests can assert Send encoding (incl. Kind=="step") end to end;
// setReplyMode is echoed as inbound "replymode:<mode>".

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestHelperPluginProcess is not a test: it is the mock plugin's main(),
// entered only when the supervisor re-execs the test binary.
func TestHelperPluginProcess(t *testing.T) {
	if os.Getenv("GO_WANT_PLUGIN_PROCESS") != "1" {
		return
	}
	switch os.Getenv("MOCK_TRANSPORT") {
	case "nats":
		runMockPluginNATS()
	default:
		runMockPluginStdio()
	}
	os.Exit(0)
}

// mockBehavior evaluates MOCK_BEHAVIOR/MOCK_MARKER: it may exit the process
// (die-then-fail relaunch) and reports whether this run should crash shortly
// after start (crash-once first run).
func mockBehavior() (crashAfterStart bool) {
	marker := os.Getenv("MOCK_MARKER")
	switch os.Getenv("MOCK_BEHAVIOR") {
	case "die-then-fail":
		if marker != "" {
			if _, err := os.Stat(marker); err == nil {
				os.Exit(1) // relaunch after the crash: fail before ready
			}
			_ = os.WriteFile(marker, []byte("first-run"), 0o644)
		}
	case "crash-once":
		if marker != "" {
			if _, err := os.Stat(marker); err != nil {
				_ = os.WriteFile(marker, []byte("crashed"), 0o644)
				return true
			}
		}
	}
	return false
}

func mockName() string {
	if n := os.Getenv("RYSH_CHANNEL_PLUGIN_NAME"); n != "" {
		return n
	}
	return "mockchan"
}

func mockHelloInbound() channels.InboundMessage {
	return channels.InboundMessage{
		SenderID:   "mock-sender",
		SenderName: "Mock",
		Content:    "hello-from-plugin",
		ThreadID:   "t-1",
		Metadata:   map[string]string{"k": "v"},
	}
}

func mockEchoInbound(om channels.OutboundMessage) channels.InboundMessage {
	return channels.InboundMessage{
		SenderID: "mock-sender",
		Content:  "echo:" + om.Kind + ":" + om.Content,
		ThreadID: om.ThreadID,
	}
}

// --- stdio flavor -----------------------------------------------------------

func runMockPluginStdio() {
	crashAfterStart := mockBehavior()
	name := mockName()

	writeLine := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		_, _ = os.Stdout.Write(append(b, '\n'))
	}
	notify := func(method string, params any) {
		raw, _ := json.Marshal(params)
		writeLine(map[string]any{"jsonrpc": "2.0", "method": method, "params": json.RawMessage(raw)})
	}

	notify("ready", map[string]any{})

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), pluginMaxLine)
	for scanner.Scan() {
		var f pluginRPCFrame
		if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
			continue
		}
		reply := func(result any) {
			raw, _ := json.Marshal(result)
			writeLine(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": json.RawMessage(raw)})
		}
		switch f.Method {
		case "start":
			reply(map[string]any{"ok": true})
			notify("status", msg.ChannelStatus{Type: name, Connected: true, Details: "mock connected"})
			notify("inbound", mockHelloInbound())
			if crashAfterStart {
				go func() {
					time.Sleep(150 * time.Millisecond)
					os.Exit(1)
				}()
			}
		case "send":
			var om channels.OutboundMessage
			_ = json.Unmarshal(f.Params, &om)
			if om.Content == "__die__" {
				os.Exit(1)
			}
			reply(map[string]any{"ok": true})
			notify("inbound", mockEchoInbound(om))
		case "status":
			reply(msg.ChannelStatus{Type: name, Connected: true, Details: "mock status"})
		case "stop":
			reply(map[string]any{"ok": true})
			os.Exit(0)
		case "setReplyMode":
			var p setReplyModeParams
			_ = json.Unmarshal(f.Params, &p)
			notify("inbound", channels.InboundMessage{SenderID: "mock-sender", Content: "replymode:" + p.Mode})
		}
	}
	os.Exit(0) // stdin closed → clean shutdown
}

// --- nats flavor ------------------------------------------------------------

func runMockPluginNATS() {
	crashAfterStart := mockBehavior()
	name := mockName()
	prefix := pluginSubjectPrefix + name

	nc, err := nats.Connect(os.Getenv("RYSH_PLUGIN_NATS_URL"))
	if err != nil {
		os.Exit(1)
	}
	defer nc.Close()

	pub := func(suffix string, v any) {
		b, _ := json.Marshal(v)
		_ = nc.Publish(prefix+suffix, b)
	}
	ack := []byte(`{"ok":true}`)

	_, _ = nc.Subscribe(prefix+".start", func(m *nats.Msg) {
		_ = m.Respond(ack)
		pub(".status", msg.ChannelStatus{Type: name, Connected: true, Details: "mock connected"})
		pub(".inbound", mockHelloInbound())
		if crashAfterStart {
			go func() {
				time.Sleep(150 * time.Millisecond)
				os.Exit(1)
			}()
		}
	})
	_, _ = nc.Subscribe(prefix+".send", func(m *nats.Msg) {
		var om channels.OutboundMessage
		_ = json.Unmarshal(m.Data, &om)
		if om.Content == "__die__" {
			os.Exit(1)
		}
		_ = m.Respond(ack)
		pub(".inbound", mockEchoInbound(om))
	})
	_, _ = nc.Subscribe(prefix+".status", func(m *nats.Msg) {
		if m.Reply == "" {
			return // our own published status update, not a request
		}
		b, _ := json.Marshal(msg.ChannelStatus{Type: name, Connected: true, Details: "mock status"})
		_ = m.Respond(b)
	})
	_, _ = nc.Subscribe(prefix+".stop", func(m *nats.Msg) {
		_ = m.Respond(ack)
		go func() {
			time.Sleep(50 * time.Millisecond)
			os.Exit(0)
		}()
	})
	_, _ = nc.Subscribe(prefix+".control", func(m *nats.Msg) {
		var p setReplyModeParams
		_ = json.Unmarshal(m.Data, &p)
		if p.Op == "set_reply_mode" {
			pub(".inbound", channels.InboundMessage{SenderID: "mock-sender", Content: "replymode:" + p.Mode})
		}
	})
	_ = nc.Flush()
	pub(".ready", map[string]any{})
	_ = nc.Flush()
	select {} // serve until killed/stopped
}
