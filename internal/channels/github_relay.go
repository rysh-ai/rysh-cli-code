// SPDX-License-Identifier: Apache-2.0

package channels

// Relay mode runs the GitHub channel through rysh-server (E-108, D-41). The
// server owns the GitHub App: its webhook secret verifies every delivery, its
// private key mints the installation tokens, and it posts replies and the 👀
// reaction. This session holds neither: it dials out, receives the verified
// deliveries, and asks the server to post. Every design-073 guard still runs
// here, on the verbatim payload the server forwards.
//
//	inbound   ws.{workspace}.github.{connection}.inbound   (server -> session, durable)
//	outbound  ws.{workspace}.github.{connection}.outbound  (session -> server, request/reply)
//	replay    ws.{workspace}.github.{connection}.replay    (request/reply)
//
// The transport, the holder lease (D-21), the watermark and the outbound
// idempotency are WhatsApp relay mode's (whatsapp_relay.go); only the payload
// handling is GitHub's.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/upstream"
)

// githubRelayState is the channel-state directory relay mode keeps its
// watermark, handled set and holder id in.
const githubRelayState = "github-relay"

// githubRelayHolderInUseError is the status error while another session holds
// the connection (D-21), worded as WhatsApp's.
const githubRelayHolderInUseError = "This GitHub connection is already being served by another rysh session (another directory or machine). Stop that session, or wait up to 90 s after it exits, then restart this channel."

// RelayMode reports whether the adapter runs through rysh-server's channel
// relay rather than its own webhook listener and REST token.
func (g *GitHubAdapter) RelayMode() bool { return g.config.Relay }

func (g *GitHubAdapter) relaySubject(suffix string) string {
	return fmt.Sprintf("ws.%s.github.%s.%s", g.config.RelayWorkspaceID, g.config.RelayConnectionID, suffix)
}

// checkRelayConfig is checkConfig for relay mode. The App's secret and token
// live on the server, so either one set here is refused: a value that is
// never used reads as protection it does not give.
func (g *GitHubAdapter) checkRelayConfig() error {
	if strings.TrimSpace(g.config.AppSecret) != "" {
		return fmt.Errorf("github: app_secret must not be set in relay mode — rysh-server holds the App's webhook secret and verifies every delivery")
	}
	if strings.TrimSpace(g.config.BotToken) != "" {
		return fmt.Errorf("github: bot_token must not be set in relay mode — rysh-server posts replies and reactions as the App")
	}
	if g.config.RelayURL == "" || g.config.RelayAPIKey == "" {
		return fmt.Errorf("github relay: upstream url and api key are required (an enabled [upstream] block)")
	}
	if g.config.RelayWorkspaceID == "" || g.config.RelayConnectionID == "" {
		return fmt.Errorf("github relay: workspace and connection id are required (the workspace's GitHub connection on rysh-server)")
	}
	return g.checkTriggerConfig()
}

// startRelay dials the upstream server and subscribes to this connection's
// deliveries. No local listener is bound.
func (g *GitHubAdapter) startRelay(ctx context.Context) error {
	cfg := g.config
	g.mu.Lock()
	g.relayStopping = false
	g.mu.Unlock()

	wsURL := cfg.RelayURL
	switch {
	case strings.HasPrefix(wsURL, "https://"):
		wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
	case strings.HasPrefix(wsURL, "http://"):
		wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
	}
	workspacePath := cfg.RelayWorkspace
	if workspacePath == "" {
		workspacePath = cfg.RelayWorkspaceID
	}
	holder := g.relayHolder()

	opts := append([]nats.Option{
		nats.Name("rysh-github-relay"),
		nats.ProxyPath(relayProxyPath(workspacePath, holder)),
		upstream.NATSAPIKeyHeader(cfg.RelayAPIKey), // the key rides in a header, not the URL (F-86)
		nats.Token(cfg.RelayAPIKey),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
	}, g.relayEventHandlers()...)
	nc, err := nats.Connect(wsURL, opts...)
	if err != nil {
		return fmt.Errorf("github relay: connect upstream: %w", err)
	}
	if err := g.attachRelay(nc); err != nil {
		nc.Close()
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	g.cancel = cancel
	go func() {
		<-ctx.Done()
		g.mu.Lock()
		c := g.relayConn
		g.relayConn = nil
		g.connected = false
		g.mu.Unlock()
		if c != nil {
			c.Close()
		}
		slog.Info("github relay: adapter stopped")
	}()

	slog.Info("github relay: ready", "upstream", cfg.RelayURL, "inbound", g.relaySubject("inbound"),
		"holder", holder, "repos", cfg.Channels, "trigger", "@"+g.botLogin)
	return nil
}

// relayEventHandlers are the connection callbacks of a relay session.
func (g *GitHubAdapter) relayEventHandlers() []nats.Option {
	return []nats.Option{
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			slog.Warn("github relay: disconnected from upstream", "err", err)
			g.mu.Lock()
			g.connected = false
			g.mu.Unlock()
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			slog.Info("github relay: reconnected to upstream")
			g.mu.Lock()
			g.connected = true
			g.mu.Unlock()
			// Deliveries published while away are replayed, and a connection
			// another session held is tried again (D-21).
			g.goRelay(g.retryRelayHold)
		}),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			g.handleRelayAsyncError(err)
		}),
	}
}

// attachRelay subscribes a connected session to its inbound subject, makes it
// the adapter's relay connection and starts the catch-up replay.
func (g *GitHubAdapter) attachRelay(nc *nats.Conn) error {
	g.mu.Lock()
	g.connected = true
	g.err = ""
	g.relayRefused = false
	g.mu.Unlock()

	inSubj := g.relaySubject("inbound")
	if _, err := nc.QueueSubscribe(inSubj, relayQueueGroup, func(m *nats.Msg) {
		g.handleRelayInbound(m.Data, false) // live delivery
	}); err != nil {
		return fmt.Errorf("github relay: subscribe %s: %w", inSubj, err)
	}

	g.mu.Lock()
	g.relayConn = nc
	g.mu.Unlock()

	g.goRelay(g.replayMissed)
	return nil
}

// goRelay runs f in the background as tracked relay work, so Stop returns only
// after it has finished. Once Stop has begun nothing new starts: the check and
// the Add happen under mu, so no Add can race Stop's Wait.
func (g *GitHubAdapter) goRelay(f func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.relayStopping {
		return
	}
	g.relayWork.Add(1)
	go func() {
		defer g.relayWork.Done()
		f()
	}()
}

// relayHolder returns this session's holder id for the connection lease,
// derived from the working directory and persisted beside the watermark.
func (g *GitHubAdapter) relayHolder() string {
	dir, err := os.Getwd()
	if err == nil {
		dir, err = filepath.Abs(dir)
	}
	if err != nil {
		slog.Warn("github relay: working directory unknown; the proxy assigns an anonymous holder", "err", err)
		return ""
	}
	id := relayHolderID(dir, g.config.RelayWorkspaceID)
	key := g.config.RelayConnectionID + ".holder"
	var st relayHolderState
	if !loadChannelState(githubRelayState, key, &st) || st.Holder != id {
		if err := saveChannelState(githubRelayState, key, relayHolderState{Holder: id}); err != nil {
			slog.Warn("github relay: persist holder id failed", "holder", id, "err", err)
		}
	}
	return id
}

// handleRelayAsyncError: a permissions violation naming this connection's own
// …inbound or …replay subject means another session holds the connection.
func (g *GitHubAdapter) handleRelayAsyncError(err error) {
	if err == nil {
		return
	}
	text := err.Error()
	own := strings.Contains(text, `"`+g.relaySubject("inbound")+`"`) || strings.Contains(text, `"`+g.relaySubject("replay")+`"`)
	if !errors.Is(err, nats.ErrPermissionViolation) || !own {
		slog.Warn("github relay: upstream error", "err", err)
		return
	}
	g.mu.Lock()
	first := !g.relayRefused
	g.relayRefused = true
	g.relayRefusals++
	g.err = githubRelayHolderInUseError
	g.mu.Unlock()
	if first {
		slog.Warn("github relay: connection held by another rysh session; not serving it",
			"connection", g.config.RelayConnectionID, "err", err)
	}
}

// retryRelayHold runs on every reconnect: the replay request is the second
// half of re-trying the lease.
func (g *GitHubAdapter) retryRelayHold() {
	g.mu.Lock()
	g.relayRefused = false
	g.mu.Unlock()
	g.replayMissed()
}

// loadRelayState restores the replay watermark and the handled set, so a
// restart neither asks for everything nor answers a delivery twice (replay
// re-sends from a one-second watermark).
func (g *GitHubAdapter) loadRelayState() {
	conn := g.config.RelayConnectionID
	var wm relayWatermark
	if loadChannelState(githubRelayState, conn, &wm) {
		g.mu.Lock()
		if wm.LastSeen > g.relayLastSeen {
			g.relayLastSeen = wm.LastSeen
		}
		g.mu.Unlock()
	}
	var handled []string
	if loadChannelState(githubRelayState, conn+".handled", &handled) {
		g.markHandled(handled...)
	}
}

// persistHandled saves the handled set. Best effort, like the watermark.
func (g *GitHubAdapter) persistHandled() {
	g.mu.RLock()
	keys := append([]string(nil), g.handledOrder...)
	g.mu.RUnlock()
	if err := saveChannelState(githubRelayState, g.config.RelayConnectionID+".handled", keys); err != nil {
		slog.Warn("github relay: persist handled set failed", "err", err)
	}
}

// noteRelaySeen advances and persists the replay watermark.
func (g *GitHubAdapter) noteRelaySeen(ts int64) {
	g.mu.Lock()
	if ts <= g.relayLastSeen {
		g.mu.Unlock()
		return
	}
	g.relayLastSeen = ts
	g.mu.Unlock()
	if err := saveChannelState(githubRelayState, g.config.RelayConnectionID, relayWatermark{LastSeen: ts}); err != nil {
		slog.Warn("github relay: persist watermark failed", "err", err)
	}
}

// handleRelayInbound turns one relayed delivery into an inbound message under
// exactly the rules the webhook path applies (parseGitHubTrigger, refuse,
// markHandled, inboundFor). The server's envelope only routes: the decision
// is made on the verbatim payload.
func (g *GitHubAdapter) handleRelayInbound(data []byte, replayed bool) {
	var in relayInbound
	if err := json.Unmarshal(data, &in); err != nil {
		slog.Warn("github relay: bad inbound payload", "err", err)
		return
	}
	if in.ChannelType != "github" {
		slog.Warn("github relay: inbound for another channel type; dropped", "channel_type", in.ChannelType)
		return
	}
	event := in.Metadata["github_event"]
	switch event {
	case "issue_comment", "issues", "pull_request_review_comment":
	default:
		slog.Debug("github relay: ignoring event", "event", event)
		g.noteRelaySeen(in.Timestamp)
		return
	}
	t, ok := parseGitHubTrigger(event, []byte(in.Metadata["github_payload"]))
	if !ok {
		g.noteRelaySeen(in.Timestamp)
		return
	}
	// The envelope's repository must be the payload's: the server routes on
	// one, the guards read the other.
	if !strings.EqualFold(in.Metadata["github_repo"], t.repo) {
		slog.Warn("github relay: envelope repository differs from the payload's; dropped",
			"envelope", in.Metadata["github_repo"], "payload", t.repo)
		g.noteRelaySeen(in.Timestamp)
		return
	}
	if reason := g.refuse(t); reason != "" {
		slog.Debug("github relay: not acting on delivery", "event", event, "repo", t.repo,
			"number", t.number, "author", t.author.Login, "reason", reason)
		g.noteRelaySeen(in.Timestamp)
		return
	}
	if !g.markHandled(in.MessageID, t.dedupeKey) {
		slog.Debug("github relay: ignoring repeated delivery", "delivery", in.MessageID, "key", t.dedupeKey)
		g.noteRelaySeen(in.Timestamp)
		return
	}
	g.persistHandled()
	g.noteRelaySeen(in.Timestamp)

	g.mu.RLock()
	refused := g.relayRefused
	g.mu.RUnlock()
	if refused {
		slog.Info("github relay: connection held by another session — not answered",
			"delivery", in.MessageID, "thread", fmt.Sprintf("%s#%d", t.repo, t.number))
		return
	}
	// A mention replayed long after it was written is not answered: the
	// thread has moved on, and a session starting fresh would otherwise
	// answer everything the server's stream still holds.
	if replayed && relayTooStale(in.Timestamp) {
		slog.Warn("github relay: replayed mention too old to answer",
			"delivery", in.MessageID, "thread", fmt.Sprintf("%s#%d", t.repo, t.number), "cutoff", relayReplyWindow)
		return
	}

	im := g.inboundFor(t, in.MessageID)
	if replayed {
		im.Metadata["replayed"] = "true"
	}
	select {
	case g.inbound <- im:
	default:
		slog.Warn("github relay: inbound buffer full, dropping mention", "thread", im.ThreadID)
		return
	}
	g.goRelay(func() { g.reactRelay(t) })
}

// reactRelay asks the server for the 👀 pickup reaction on the triggering
// comment. A new issue carries no comment, and the relay contract has no
// issue reaction, so an issue is not marked.
func (g *GitHubAdapter) reactRelay(t githubTrigger) {
	if t.commentID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := g.publishRelay(ctx, relayOutbound{
		ConnectionID: g.config.RelayConnectionID,
		WorkspaceID:  g.config.RelayWorkspaceID,
		Type:         "reaction",
		RecipientID:  t.repo,
		ThreadID:     fmt.Sprintf("%s:%d", t.event, t.commentID),
		Content:      "eyes",
		MessageID:    relayChunkID(newRelayMessageID(), 0),
	})
	if err != nil {
		slog.Warn("github relay: reaction failed", "repo", t.repo, "comment", t.commentID, "err", err)
	}
}

// sendRelayComment asks the server to post content on repo#number, split as
// the REST path splits it; each chunk carries its own idempotency key.
func (g *GitHubAdapter) sendRelayComment(ctx context.Context, repo string, number int, content string) error {
	base := newRelayMessageID()
	for i, chunk := range splitMessage(content, githubMaxBodyLen) {
		if err := g.publishRelay(ctx, relayOutbound{
			ConnectionID: g.config.RelayConnectionID,
			WorkspaceID:  g.config.RelayWorkspaceID,
			RecipientID:  fmt.Sprintf("%s#%d", repo, number),
			Content:      chunk,
			MessageID:    relayChunkID(base, i),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (g *GitHubAdapter) publishRelay(ctx context.Context, out relayOutbound) error {
	g.mu.RLock()
	nc := g.relayConn
	g.mu.RUnlock()
	if nc == nil {
		return fmt.Errorf("github relay: not connected to upstream")
	}
	return requestRelay(ctx, nc, g.relaySubject("outbound"), "github relay", out)
}

// replayMissed asks the server for deliveries published while this session
// was not connected, since the watermark.
func (g *GitHubAdapter) replayMissed() {
	g.mu.RLock()
	nc, since := g.relayConn, g.relayLastSeen
	refused, refusals := g.relayRefused, g.relayRefusals
	g.mu.RUnlock()
	if nc == nil || refused {
		return
	}
	req, err := json.Marshal(relayReplayRequest{SinceTimestamp: since})
	if err != nil {
		return
	}
	reply, err := nc.Request(g.relaySubject("replay"), req, relayReplyTimeout)
	if err != nil {
		slog.Warn("github relay: replay request failed", "err", err)
		return
	}
	var resp relayReplayResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		slog.Warn("github relay: bad replay response", "err", err)
		return
	}
	if resp.Error != "" {
		slog.Warn("github relay: replay refused", "err", resp.Error)
		return
	}
	g.mu.Lock()
	if g.relayRefusals == refusals && !g.relayRefused && g.err == githubRelayHolderInUseError {
		g.err = ""
		slog.Info("github relay: connection held by this session again", "connection", g.config.RelayConnectionID)
	}
	g.mu.Unlock()
	if len(resp.Messages) == 0 {
		return
	}
	slog.Info("github relay: replaying missed deliveries", "count", len(resp.Messages), "since", since, "truncated", resp.Truncated)
	for _, raw := range resp.Messages {
		g.handleRelayInbound(raw, true)
	}
}
