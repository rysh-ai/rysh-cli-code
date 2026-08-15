// SPDX-License-Identifier: Apache-2.0

package channels

// SignalAdapter — C4 (openclaw_roadmap design 001 §4.4). In-core JSON-RPC
// client for a `signal-cli` daemon sidecar (UNIX socket or TCP).
//
// Transport: newline-delimited JSON-RPC 2.0 on a single connection. rysh
// issues "send" requests (matched to responses by id) and the daemon pushes
// unsolicited "receive" notifications for inbound envelopes on the same
// connection. One reader goroutine decodes line-by-line and dispatches;
// writes are serialized with a mutex; the connection reconnects with
// exponential backoff (same discipline as SlackAdapter.connectionLoop).
//
// Daemon ownership: attach-only is the documented default (design 001 §8) —
// rysh connects to an externally managed `signal-cli daemon` at SidecarAddr
// and does not manage a JVM lifecycle. As a convenience, when SidecarCmd is
// set rysh spawns the daemon on Start and kills it on Stop.
//
// Device-link QR pairing (`signal-cli link` → sgnl:// URI → QR scan) is
// design 003 / a separate task — this adapter assumes the number is already
// registered/linked under signal-cli's data dir.
//
// Honest limits (§4.4): signal-cli is a community/unofficial JVM tool run as
// a sidecar; the linked device shares the account with the phone and a
// lost/re-registered primary can unlink it; there are no formal published
// rate limits but aggressive send can trip abuse throttles, so sends are
// paced modestly. Labeled constrained.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// signalRPCTimeout bounds how long Send waits for the daemon's response
	// to a "send" RPC.
	signalRPCTimeout = 10 * time.Second
	// signalLinkTimeout bounds how long finishLink waits for the phone to scan
	// the device-link QR and approve the new device (X4, design 009).
	signalLinkTimeout = 5 * time.Minute
	// signalSendMinInterval paces outbound sends (§4.4: no formal limits,
	// but aggressive send can trip Signal abuse throttles).
	signalSendMinInterval = 300 * time.Millisecond
	// signalMaxLine caps a single JSON-RPC line from the daemon (envelopes
	// with previews/quotes can be large).
	signalMaxLine = 1024 * 1024
)

// SignalAdapter bridges Signal (via a signal-cli daemon) to the humanoid
// actor system.
type SignalAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage

	mu        sync.RWMutex // guards connected, lastErr, details, replyMode, conn, cancel, child
	connected bool
	lastErr   string
	details   string
	replyMode string
	conn      net.Conn
	cancel    context.CancelFunc
	child     *exec.Cmd // non-nil only when rysh spawned the daemon

	// writeMu serializes writes to the connection and enforces send pacing.
	writeMu  sync.Mutex
	lastSend time.Time

	// pendingMu guards the pending request map and the id counter.
	pendingMu sync.Mutex
	pending   map[string]chan signalRPCFrame
	nextID    uint64

	// pairingCh carries device-link lifecycle events (X4, design 009) while the
	// signal-cli link flow runs. Buffered so linkDevice never blocks on a slow
	// consumer.
	pairingCh chan PairingEvent

	// linkMu guards linkInFlight and linkCtx: one link flow at a time, bound to
	// the adapter lifetime so Stop cancels an in-flight finishLink wait.
	linkMu       sync.Mutex
	linkInFlight bool
	linkCtx      context.Context
}

// NewSignalAdapter creates a Signal channel adapter from the config.
func NewSignalAdapter(config msg.ChannelConfig) *SignalAdapter {
	rm := config.ReplyMode
	if rm == "" {
		rm = "messages"
	}
	return &SignalAdapter{
		config:    config,
		inbound:   make(chan InboundMessage, 100),
		replyMode: rm,
		pending:   make(map[string]chan signalRPCFrame),
		pairingCh: make(chan PairingEvent, 8),
	}
}

// Type returns "signal".
func (s *SignalAdapter) Type() string { return "signal" }

// PairingCh implements PairingChannel: it emits device-link lifecycle events
// (qr / linked / error) while the signal-cli link flow runs (X4, design 009).
func (s *SignalAdapter) PairingCh() <-chan PairingEvent { return s.pairingCh }

var _ PairingChannel = (*SignalAdapter)(nil)
var _ LinkableChannel = (*SignalAdapter)(nil)

// TriggerLink implements LinkableChannel: it starts the device-link flow on
// demand (`##humanoid pair link`, design 009 §3.4). It returns immediately —
// progress and the re-link-guard refusal both arrive as pairing events. The
// only synchronous errors are "not connected" and "already linking".
func (s *SignalAdapter) TriggerLink(force bool) error {
	s.mu.RLock()
	connected := s.connected
	s.mu.RUnlock()
	if !connected {
		return fmt.Errorf("signal: not connected — start the channel first")
	}
	s.linkMu.Lock()
	if s.linkInFlight {
		s.linkMu.Unlock()
		return fmt.Errorf("signal: a device-link flow is already in progress")
	}
	s.linkInFlight = true
	ctx := s.linkCtx
	s.linkMu.Unlock()
	if ctx == nil { // Start never ran; defensive — connected implies it did.
		s.linkMu.Lock()
		s.linkInFlight = false
		s.linkMu.Unlock()
		return fmt.Errorf("signal: adapter not started")
	}
	go s.runLink(ctx, force)
	return nil
}

// runLink applies the re-link guard then runs linkDevice, clearing the
// in-flight flag when done. Never auto-relink an already-provisioned number
// (design 009 §6): unless force is set, a daemon that already holds a linked
// account turns the request into an "error" pairing event instead of a link.
func (s *SignalAdapter) runLink(ctx context.Context, force bool) {
	defer func() {
		s.linkMu.Lock()
		s.linkInFlight = false
		s.linkMu.Unlock()
	}()
	if !force {
		if acct, linked := s.accountLinked(ctx); linked {
			s.emitPairing(ctx, PairingEvent{Kind: "error", Detail: fmt.Sprintf(
				"already linked (account %s) — re-linking would re-provision the device; use force to override", acct)})
			return
		}
	}
	s.linkDevice(ctx)
}

// accountLinked asks the daemon (listAccounts) whether it already holds a
// registered/linked account, returning the first account id when it does.
// Errors and unrecognised responses report "not linked": the guard is a
// convenience rail, and older daemons without listAccounts must not lose the
// explicitly requested link flow.
func (s *SignalAdapter) accountLinked(ctx context.Context) (string, bool) {
	resp, err := s.call(ctx, "listAccounts", nil)
	if err != nil || resp.Error != nil {
		return "", false
	}
	var accounts []struct {
		Number string `json:"number"`
	}
	if err := json.Unmarshal(resp.Result, &accounts); err != nil || len(accounts) == 0 {
		return "", false
	}
	if accounts[0].Number != "" {
		return accounts[0].Number, true
	}
	return "an existing account", true
}

// linkDevice runs the signal-cli device-link flow (X4, design 009): startLink
// yields a device-link URI (emitted as a "qr" event to render and scan), then
// finishLink blocks until the phone approves (emitted as "linked", or "error").
// It runs on its own goroutine, spawned from Start only when config.Link is set,
// and requires an account-less daemon (started without -a) to expose the methods.
func (s *SignalAdapter) linkDevice(ctx context.Context) {
	start, err := s.call(ctx, "startLink", nil)
	if err != nil {
		s.emitPairing(ctx, PairingEvent{Kind: "error", Detail: "startLink: " + err.Error()})
		return
	}
	if start.Error != nil {
		s.emitPairing(ctx, PairingEvent{Kind: "error", Detail: "startLink: " + start.Error.Message})
		return
	}
	var sr struct {
		DeviceLinkURI string `json:"deviceLinkUri"`
	}
	if err := json.Unmarshal(start.Result, &sr); err != nil || sr.DeviceLinkURI == "" {
		s.emitPairing(ctx, PairingEvent{Kind: "error", Detail: "startLink: no deviceLinkUri in response"})
		return
	}
	s.emitPairing(ctx, PairingEvent{Kind: "qr", QR: sr.DeviceLinkURI})

	// finishLink blocks until the user scans and approves, so give it the long
	// link timeout rather than the short per-message one.
	fin, err := s.callWithTimeout(ctx, "finishLink",
		map[string]any{"deviceLinkUri": sr.DeviceLinkURI, "deviceName": "rysh"},
		signalLinkTimeout)
	if err != nil {
		s.emitPairing(ctx, PairingEvent{Kind: "error", Detail: "finishLink: " + err.Error()})
		return
	}
	if fin.Error != nil {
		s.emitPairing(ctx, PairingEvent{Kind: "error", Detail: "finishLink: " + fin.Error.Message})
		return
	}
	// The result identifies the newly linked account; persist it verbatim as the
	// device-link session (the humanoid stores it as a secret and never logs it).
	s.emitPairing(ctx, PairingEvent{Kind: "linked", Session: fin.Result})
}

// emitPairing delivers a device-link event on PairingCh without blocking past
// context cancellation (the channel is buffered; a stopped adapter drops it).
func (s *SignalAdapter) emitPairing(ctx context.Context, ev PairingEvent) {
	select {
	case s.pairingCh <- ev:
	case <-ctx.Done():
	}
}

// Start dials the signal-cli JSON-RPC endpoint, spawning the daemon first
// when SidecarCmd is configured, and launches the reader/reconnect loop.
func (s *SignalAdapter) Start(ctx context.Context) error {
	slog.Debug("signal: starting adapter",
		"number", s.config.Number,
		"sidecar_addr", s.config.SidecarAddr,
		"has_sidecar_cmd", s.config.SidecarCmd != "")

	if s.config.SidecarAddr == "" {
		return fmt.Errorf("signal: sidecar_addr is required")
	}
	// The number is discovered by the link flow, so it is not required when
	// linking (X4, design 009); every other mode needs it up front.
	if s.config.Number == "" && !s.config.Link {
		return fmt.Errorf("signal: number is required")
	}

	ctx, cancel := context.WithCancel(ctx)

	// Spawn the sidecar daemon when configured (attach-only is the default:
	// SidecarCmd unset means an externally managed daemon owns the socket).
	var child *exec.Cmd
	dialAttempts := 3
	if cmdline := strings.TrimSpace(s.config.SidecarCmd); cmdline != "" {
		fields := strings.Fields(cmdline)
		child = exec.Command(fields[0], fields[1:]...)
		if err := child.Start(); err != nil {
			cancel()
			return fmt.Errorf("signal: spawn sidecar: %w", err)
		}
		// Reap the child when it exits so it never zombies.
		go func() { _ = child.Wait() }()
		slog.Info("signal: sidecar spawned", "pid", child.Process.Pid, "cmd", fields[0])
		// A freshly spawned daemon needs time to create its socket.
		dialAttempts = 10
	}

	conn, err := s.dialRetry(ctx, dialAttempts)
	if err != nil {
		if child != nil && child.Process != nil {
			_ = child.Process.Kill()
		}
		cancel()
		return err
	}

	details := "attached to " + s.config.SidecarAddr
	if child != nil {
		details = fmt.Sprintf("sidecar spawned pid %d, attached to %s",
			child.Process.Pid, s.config.SidecarAddr)
	}

	// linkCtx must be visible before connected flips true: TriggerLink gates on
	// connected, then reads linkCtx.
	s.linkMu.Lock()
	s.linkCtx = ctx
	s.linkMu.Unlock()

	s.mu.Lock()
	s.conn = conn
	s.cancel = cancel
	s.child = child
	s.connected = true
	s.lastErr = ""
	s.details = details
	s.mu.Unlock()

	go s.connectionLoop(ctx, conn)

	// Device-link flow (X4, design 009): when linking is requested, run it on a
	// side goroutine so Start returns promptly; it emits QR/linked/error on
	// PairingCh, which the humanoid's pairingLoop renders and persists. It runs
	// through the same re-link guard as `##humanoid pair link` — a daemon that
	// already holds an account refuses instead of re-provisioning (§6).
	if s.config.Link {
		s.linkMu.Lock()
		s.linkInFlight = true
		s.linkMu.Unlock()
		go s.runLink(ctx, false)
	}

	slog.Info("signal: adapter started", "addr", s.config.SidecarAddr, "spawned", child != nil)
	return nil
}

// Stop closes the sidecar connection and kills the daemon if rysh spawned it.
func (s *SignalAdapter) Stop() error {
	slog.Debug("signal: stopping adapter")

	s.mu.Lock()
	cancel := s.cancel
	conn := s.conn
	child := s.child
	s.cancel = nil
	s.conn = nil
	s.child = nil
	s.connected = false
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	if child != nil && child.Process != nil {
		_ = child.Process.Kill() // reaped by the Wait goroutine from Start
	}
	s.failPending()

	slog.Info("signal: adapter stopped")
	return nil
}

// Send delivers an outbound message via a JSON-RPC "send" to the daemon.
func (s *SignalAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	// §4.7 rendering rule: Signal is a DM/mobile channel, so progress-step
	// titles (Kind == "step") are SUPPRESSED entirely — returning nil without
	// sending — so the user's phone is not buzzed for every tool call. The
	// final reply always sends. Per-adapter constant, overridable later if
	// demand appears.
	if outbound.Kind == OutboundKindStep {
		slog.Debug("signal: suppressing step message (mobile channel)",
			"content_len", len(outbound.Content))
		return nil
	}

	params := map[string]any{
		"account": s.config.Number,
		"message": outbound.Content,
	}
	for k, v := range signalRecipientParams(outbound.RecipientID, outbound.ThreadID) {
		params[k] = v
	}

	resp, err := s.call(ctx, "send", params)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("signal: send rejected: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}

	slog.Info("signal: message sent",
		"recipient", outbound.RecipientID,
		"thread", outbound.ThreadID,
		"len", len(outbound.Content))
	return nil
}

// InboundCh returns the inbound message channel.
func (s *SignalAdapter) InboundCh() <-chan InboundMessage { return s.inbound }

// Status reports the connection status.
func (s *SignalAdapter) Status() msg.ChannelStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return msg.ChannelStatus{
		Type:      "signal",
		Connected: s.connected,
		Error:     s.lastErr,
		Details:   s.details,
	}
}

// SetReplyMode changes the reply mode at runtime. 1:1 messages are always
// processed; in "mentions" mode group messages that do not @-mention the
// linked number are forwarded observe_only (mirroring Slack).
func (s *SignalAdapter) SetReplyMode(mode string) {
	s.mu.Lock()
	s.replyMode = mode
	s.mu.Unlock()
	slog.Info("signal: reply mode changed", "mode", mode)
}

// --- connection management -------------------------------------------------

// signalDialTarget resolves SidecarAddr into a dial network + address:
// a path starting with "/" or containing no ":" is a UNIX socket, anything
// else is host:port TCP.
func signalDialTarget(addr string) (network, address string) {
	if strings.HasPrefix(addr, "/") || !strings.Contains(addr, ":") {
		return "unix", addr
	}
	return "tcp", addr
}

// dialRetry dials the sidecar endpoint, retrying a few times (a freshly
// spawned daemon needs a moment to create its socket).
func (s *SignalAdapter) dialRetry(ctx context.Context, attempts int) (net.Conn, error) {
	network, address := signalDialTarget(s.config.SidecarAddr)
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		d := net.Dialer{Timeout: 5 * time.Second}
		conn, err := d.DialContext(ctx, network, address)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		slog.Debug("signal: dial attempt failed",
			"attempt", i+1, "network", network, "addr", address, "err", err)
	}
	return nil, fmt.Errorf("signal: dial %s %s: %w", network, address, lastErr)
}

// connectionLoop runs the reader for the current connection and reconnects
// with exponential backoff when it drops (mirrors SlackAdapter's loop). It
// exits only when the context is cancelled (i.e., Stop() was called).
func (s *SignalAdapter) connectionLoop(ctx context.Context, conn net.Conn) {
	const (
		initialBackoff = 2 * time.Second
		maxBackoff     = 60 * time.Second
	)
	backoff := initialBackoff

	for {
		// Close the connection on context cancellation so the blocked reader
		// unblocks; watchDone stops the watcher when the reader exits first.
		watchDone := make(chan struct{})
		go func(c net.Conn) {
			select {
			case <-ctx.Done():
				_ = c.Close()
			case <-watchDone:
			}
		}(conn)

		err := s.readLoop(conn)
		close(watchDone)
		_ = conn.Close()
		s.failPending()

		if ctx.Err() != nil {
			slog.Debug("signal: context cancelled, connection loop exiting")
			return
		}

		errMsg := "connection closed"
		if err != nil && err != io.EOF {
			errMsg = err.Error()
		}
		s.mu.Lock()
		s.connected = false
		s.lastErr = errMsg
		s.conn = nil
		s.mu.Unlock()
		slog.Warn("signal: sidecar connection lost, will reconnect",
			"err", errMsg, "backoff", backoff)

		// Redial with capped exponential backoff until success or shutdown.
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}

			c, derr := s.dialRetry(ctx, 1)
			if derr != nil {
				if ctx.Err() != nil {
					return
				}
				s.mu.Lock()
				s.lastErr = derr.Error()
				s.mu.Unlock()
				slog.Warn("signal: reconnect dial failed", "err", derr, "backoff", backoff)
				continue
			}

			conn = c
			s.mu.Lock()
			s.conn = c
			s.connected = true
			s.lastErr = ""
			s.mu.Unlock()
			backoff = initialBackoff
			slog.Info("signal: reconnected to sidecar", "addr", s.config.SidecarAddr)
			break
		}
	}
}

// readLoop decodes newline-delimited JSON-RPC frames from the daemon until
// the connection fails. Returns io.EOF on clean close.
func (s *SignalAdapter) readLoop(conn net.Conn) error {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), signalMaxLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame signalRPCFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			slog.Warn("signal: dropping unparseable line from sidecar", "err", err)
			continue
		}
		s.dispatchFrame(frame)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

// dispatchFrame routes a decoded frame: "receive" notifications become
// inbound messages; frames with an id matching a pending request resolve
// that request; everything else is ignored (other notification methods).
func (s *SignalAdapter) dispatchFrame(frame signalRPCFrame) {
	if frame.Method == "receive" {
		s.dispatchInbound(frame.Params)
		return
	}
	if id, ok := frame.stringID(); ok {
		s.pendingMu.Lock()
		ch := s.pending[id]
		delete(s.pending, id)
		s.pendingMu.Unlock()
		if ch != nil {
			ch <- frame // buffered(1); receiver is the sole waiter
		} else {
			slog.Debug("signal: response for unknown/expired request id", "id", id)
		}
		return
	}
	slog.Debug("signal: ignoring notification", "method", frame.Method)
}

// dispatchInbound maps a "receive" notification's params onto InboundMessage
// and enqueues it (drop + warn when the buffer is full).
func (s *SignalAdapter) dispatchInbound(params json.RawMessage) {
	inMsg, ok := signalInboundToMessage(params)
	if !ok {
		// Receipts, typing indicators, reactions, etc. arrive as envelopes
		// without dataMessage.message — skip them silently.
		slog.Debug("signal: skipping non-text envelope")
		return
	}

	// Multi-account daemons tag notifications with the account; drop
	// envelopes that belong to a different linked number.
	var acct struct {
		Account string `json:"account"`
	}
	if json.Unmarshal(params, &acct) == nil &&
		acct.Account != "" && s.config.Number != "" && acct.Account != s.config.Number {
		slog.Debug("signal: skipping envelope for other account", "account", acct.Account)
		return
	}

	// Reply-mode: 1:1 always processed; in "mentions" mode a group message
	// that does not @-mention the linked number is forwarded observe_only so
	// the HumanoidActor can echo it without triggering the LLM (like Slack).
	s.mu.RLock()
	replyMode := s.replyMode
	s.mu.RUnlock()
	if replyMode == "mentions" && inMsg.Metadata["group_id"] != "" {
		mentioned := false
		for _, m := range strings.Split(inMsg.Metadata["mentions"], ",") {
			if m != "" && m == s.config.Number {
				mentioned = true
				break
			}
		}
		if !mentioned {
			inMsg.Metadata["observe_only"] = "true"
		}
	}

	slog.Info("signal: inbound message",
		"sender", inMsg.SenderID, "thread", inMsg.ThreadID, "len", len(inMsg.Content))

	select {
	case s.inbound <- inMsg:
	default:
		slog.Warn("signal: inbound channel full, dropping message",
			"sender", inMsg.SenderID, "thread", inMsg.ThreadID)
	}
}

// failPending fails all in-flight RPCs (connection lost); callers awaiting a
// response observe the closed channel and surface a connection-lost error.
func (s *SignalAdapter) failPending() {
	s.pendingMu.Lock()
	for id, ch := range s.pending {
		close(ch)
		delete(s.pending, id)
	}
	s.pendingMu.Unlock()
}

// call issues a JSON-RPC request and awaits the matching response. Writes are
// serialized (and paced) under writeMu; responses are matched by id via the
// pending map.
func (s *SignalAdapter) call(ctx context.Context, method string, params map[string]any) (signalRPCFrame, error) {
	return s.callWithTimeout(ctx, method, params, signalRPCTimeout)
}

// callWithTimeout is call with a caller-chosen response deadline. finishLink
// (device linking) waits on a human scanning a QR, so it needs a much longer
// timeout than the per-message signalRPCTimeout used by send.
func (s *SignalAdapter) callWithTimeout(ctx context.Context, method string, params map[string]any, timeout time.Duration) (signalRPCFrame, error) {
	s.mu.RLock()
	conn := s.conn
	connected := s.connected
	s.mu.RUnlock()
	if !connected || conn == nil {
		return signalRPCFrame{}, fmt.Errorf("signal: not connected")
	}

	s.pendingMu.Lock()
	s.nextID++
	id := fmt.Sprintf("rysh-%d", s.nextID)
	ch := make(chan signalRPCFrame, 1)
	s.pending[id] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
	}()

	line, err := json.Marshal(signalRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      id,
	})
	if err != nil {
		return signalRPCFrame{}, fmt.Errorf("signal: marshal request: %w", err)
	}
	line = append(line, '\n')

	// Serialize writes and pace sends modestly (§4.4 rate-limit note).
	s.writeMu.Lock()
	if wait := signalSendMinInterval - time.Since(s.lastSend); wait > 0 {
		time.Sleep(wait)
	}
	_, werr := conn.Write(line)
	s.lastSend = time.Now()
	s.writeMu.Unlock()
	if werr != nil {
		return signalRPCFrame{}, fmt.Errorf("signal: write %s request: %w", method, werr)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case resp, ok := <-ch:
		if !ok {
			return signalRPCFrame{}, fmt.Errorf("signal: connection lost awaiting %s response", method)
		}
		return resp, nil
	case <-timer.C:
		return signalRPCFrame{}, fmt.Errorf("signal: %s response timeout after %s", method, timeout)
	case <-ctx.Done():
		return signalRPCFrame{}, ctx.Err()
	}
}

// --- JSON-RPC wire types ---------------------------------------------------

// signalRPCRequest is an outgoing JSON-RPC 2.0 request line.
type signalRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
	ID      string         `json:"id"`
}

// signalRPCError is a JSON-RPC 2.0 error object.
type signalRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// signalRPCFrame is any incoming line from the daemon: a response (id +
// result/error) or a notification (method + params, e.g. "receive").
type signalRPCFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *signalRPCError `json:"error,omitempty"`
}

// stringID normalizes the frame's id to a string (rysh issues string ids;
// numeric ids are kept in their raw text form).
func (f *signalRPCFrame) stringID() (string, bool) {
	raw := bytes.TrimSpace(f.ID)
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str, true
	}
	return string(raw), true
}

// --- pure mapping helpers (unit-tested) ------------------------------------

// signalReceiveParams mirrors the subset of a signal-cli "receive"
// notification's params that the adapter consumes.
type signalReceiveParams struct {
	Account  string `json:"account"`
	Envelope struct {
		Source      string `json:"source"`
		SourceUUID  string `json:"sourceUuid"`
		SourceName  string `json:"sourceName"`
		Timestamp   int64  `json:"timestamp"`
		DataMessage *struct {
			Message   string `json:"message"`
			GroupInfo *struct {
				GroupID string `json:"groupId"`
			} `json:"groupInfo"`
			Mentions []struct {
				Number string `json:"number"`
				UUID   string `json:"uuid"`
			} `json:"mentions"`
		} `json:"dataMessage"`
	} `json:"envelope"`
}

// signalInboundToMessage maps a "receive" notification's params JSON onto an
// InboundMessage. Returns ok=false for envelopes without dataMessage.message
// (receipts, typing indicators, reactions, sync messages, ...).
func signalInboundToMessage(envelopeJSON []byte) (InboundMessage, bool) {
	var p signalReceiveParams
	if err := json.Unmarshal(envelopeJSON, &p); err != nil {
		return InboundMessage{}, false
	}
	env := p.Envelope
	dm := env.DataMessage
	if dm == nil || dm.Message == "" {
		return InboundMessage{}, false
	}

	senderID := env.Source
	if senderID == "" {
		senderID = env.SourceUUID
	}
	if senderID == "" {
		return InboundMessage{}, false
	}
	senderName := env.SourceName
	if senderName == "" {
		senderName = senderID
	}

	groupID := ""
	if dm.GroupInfo != nil {
		groupID = dm.GroupInfo.GroupID
	}
	threadID := groupID
	if threadID == "" {
		threadID = senderID
	}

	metadata := map[string]string{
		"timestamp":   strconv.FormatInt(env.Timestamp, 10),
		"group_id":    groupID,
		"source_uuid": env.SourceUUID,
	}
	if len(dm.Mentions) > 0 {
		parts := make([]string, 0, len(dm.Mentions))
		for _, m := range dm.Mentions {
			if m.Number != "" {
				parts = append(parts, m.Number)
			} else if m.UUID != "" {
				parts = append(parts, m.UUID)
			}
		}
		if len(parts) > 0 {
			metadata["mentions"] = strings.Join(parts, ",")
		}
	}

	return InboundMessage{
		SenderID:   senderID,
		SenderName: senderName,
		Content:    dm.Message,
		ThreadID:   threadID,
		Metadata:   metadata,
	}, true
}

// isSignalGroupID reports whether s looks like a Signal group id (base64 of
// the group's raw id, typically 44 chars ending "="). Phone numbers start
// with "+" and ACI uuids contain "-", so neither passes; a length floor
// keeps short digit-only strings from decoding as base64.
func isSignalGroupID(s string) bool {
	if len(s) < 24 || strings.HasPrefix(s, "+") {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(s)
	return err == nil
}

// signalRecipientParams builds the routing part of a "send" RPC's params:
// {"groupId": ...} when the thread/recipient is a group id, else
// {"recipient": [...]} for 1:1. ThreadID wins when set (it carries the
// conversation key the inbound message arrived on).
func signalRecipientParams(recipientID, threadID string) map[string]any {
	target := recipientID
	if threadID != "" {
		target = threadID
	}
	if isSignalGroupID(target) {
		return map[string]any{"groupId": target}
	}
	return map[string]any{"recipient": []string{target}}
}
