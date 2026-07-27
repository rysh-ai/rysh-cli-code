// Command echo-channel is the reference rysh channel plugin (docs/plugin-authoring.md).
//
// It speaks the stdio transport: newline-delimited JSON-RPC 2.0 on
// stdin/stdout, exactly what rysh's PluginSupervisor drives
// (internal/channels/plugin/stdio.go is the core-side counterpart). The nats
// transport exists too (subjects rysh.channel.plugin.{name}.*), but stdio is
// the right starting point for authors: no bus credentials, no extra
// dependency — this file imports only the standard library, so it can be
// copied out of the rysh repo and built standalone.
//
// "Echo" semantics: an echo channel has no real platform behind it, so the
// only external stimulus is rysh sending a message. Every outbound `send` is
// reflected straight back as an `inbound` notification on the same thread —
// which exercises both directions of the wire contract and makes a humanoid
// literally talk to itself.
//
// Wire contract (core → plugin requests, plugin replies by id):
//
//	start(ChannelConfig)  begin "connecting"; reply {ok}; then push status + inbounds
//	send(OutboundMessage) deliver to the platform; reply {ok} or an error object
//	status()              reply the current ChannelStatus
//	stop()                graceful shutdown; reply {ok}, then exit 0
//
// Core → plugin notification (no id, no reply):
//
//	setReplyMode({op,mode})
//
// Plugin → core notifications:
//
//	ready()                 REQUIRED once at startup — core's spawn blocks on it
//	inbound(InboundMessage) a message arriving from the platform
//	status(ChannelStatus)   connection-state updates (core caches the last one)
//
// Rules a plugin must obey:
//   - stdout is the protocol. One JSON object per line, nothing else — log to
//     stderr (core forwards it).
//   - Emit `ready` promptly: core kills the process after its ready timeout
//     (10s default).
//   - Exit 0 on `stop` and on stdin EOF. Any other exit is treated as a crash:
//     core restarts with backoff and eventually circuit-breaks.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// The wire payload types, mirroring rysh's JSON encodings
// (rysh/internal/channels.InboundMessage / OutboundMessage,
// rysh/internal/msg.ChannelStatus). Declared locally — a third-party plugin
// cannot (and should not) import rysh internals; the JSON shape IS the
// contract.

type inboundMessage struct {
	SenderID   string            `json:"sender_id"`
	SenderName string            `json:"sender_name"`
	Content    string            `json:"content"`
	ThreadID   string            `json:"thread_id"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type outboundMessage struct {
	RecipientID string `json:"recipient_id"`
	Content     string `json:"content"`
	ThreadID    string `json:"thread_id"`
	Kind        string `json:"kind,omitempty"` // "" = normal message, "step" = compact progress title
}

type channelStatus struct {
	Type      string `json:"type"`
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	Details   string `json:"details,omitempty"`
}

// frame is one JSON-RPC 2.0 line, either direction. Requests carry id+method,
// notifications carry method only, responses carry id+result/error.
type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// maxLine matches core's per-line cap; a plugin emitting a longer line gets
// it dropped on the core side.
const maxLine = 1024 * 1024

// echoPlugin holds the trivial channel state.
type echoPlugin struct {
	name      string
	replyMode string
	connected bool

	mu  sync.Mutex // serializes writes: interleaved lines corrupt the protocol
	out io.Writer
}

func main() {
	name := os.Getenv("RYSH_CHANNEL_PLUGIN_NAME") // set by the supervisor at spawn
	if name == "" {
		name = "echo"
	}
	if err := run(name, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "echo-channel:", err)
		os.Exit(1)
	}
}

// run serves the protocol until `stop` or stdin EOF. Factored from main so
// the test drives it in-process over pipes.
func run(name string, stdin io.Reader, stdout io.Writer) error {
	p := &echoPlugin{name: name, replyMode: "messages", out: stdout}

	// The ready handshake, before anything else: core's spawn blocks on it.
	p.notify("ready", struct{}{})

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			fmt.Fprintf(os.Stderr, "echo-channel: dropping unparseable line: %v\n", err)
			continue
		}
		if stop := p.handle(f); stop {
			return nil
		}
	}
	return scanner.Err() // EOF (nil) = core closed stdin: clean shutdown
}

// handle dispatches one core→plugin frame; returns true on `stop`.
func (p *echoPlugin) handle(f frame) (stop bool) {
	switch f.Method {
	case "start":
		// Params carry rysh's ChannelConfig JSON. A real plugin reads its
		// platform credentials from the ${ENV} vars it listed under
		// declares_creds; echo needs none. reply_mode arrives here too.
		var cfg struct {
			ReplyMode string `json:"reply_mode"`
		}
		_ = json.Unmarshal(f.Params, &cfg)
		if cfg.ReplyMode != "" {
			p.replyMode = cfg.ReplyMode
		}
		p.reply(f.ID, map[string]bool{"ok": true})
		// A real plugin connects to its platform here (possibly async) and
		// pushes a status update when the connection settles.
		p.connected = true
		p.notify("status", p.status())

	case "send":
		var om outboundMessage
		if err := json.Unmarshal(f.Params, &om); err != nil {
			p.replyError(f.ID, -32602, "bad OutboundMessage: "+err.Error())
			return false
		}
		if !p.connected {
			// A real plugin returns an error when its platform is down; core
			// surfaces it to the humanoid's send path.
			p.replyError(f.ID, 1, "channel not started")
			return false
		}
		p.reply(f.ID, map[string]bool{"ok": true})
		// The echo: what rysh sent out comes straight back in, same thread.
		// Kind=="step" progress titles are echoed too, prefixed so tests and
		// humans can tell them apart.
		content := om.Content
		if om.Kind != "" {
			content = "[" + om.Kind + "] " + content
		}
		p.notify("inbound", inboundMessage{
			SenderID:   "echo",
			SenderName: "Echo",
			Content:    "echo: " + content,
			ThreadID:   om.ThreadID,
		})

	case "status":
		p.reply(f.ID, p.status())

	case "stop":
		p.reply(f.ID, map[string]bool{"ok": true})
		return true

	case "setReplyMode":
		// Notification (no id → no reply). Takes effect on future behavior;
		// echo just records it and reflects it in status details.
		var params struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal(f.Params, &params)
		if params.Mode != "" {
			p.replyMode = params.Mode
		}
		p.notify("status", p.status())

	default:
		if len(f.ID) > 0 {
			p.replyError(f.ID, -32601, "unknown method "+f.Method)
		}
	}
	return false
}

func (p *echoPlugin) status() channelStatus {
	return channelStatus{
		Type:      p.name,
		Connected: p.connected,
		Details:   "echoing every send back as an inbound (reply_mode " + p.replyMode + ")",
	}
}

// writeLine marshals one frame line under the write lock.
func (p *echoPlugin) writeLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "echo-channel: marshal: %v\n", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.out.Write(append(b, '\n'))
}

func (p *echoPlugin) notify(method string, params any) {
	raw, _ := json.Marshal(params)
	p.writeLine(frame{JSONRPC: "2.0", Method: method, Params: raw})
}

func (p *echoPlugin) reply(id json.RawMessage, result any) {
	raw, _ := json.Marshal(result)
	p.writeLine(frame{JSONRPC: "2.0", ID: id, Result: raw})
}

func (p *echoPlugin) replyError(id json.RawMessage, code int, msg string) {
	p.writeLine(frame{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}
