// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ProxyAuditActor is the DURABLE audit plane for the governance proxy
// (design 001 §4.5). It is a child of WorkspaceActor, spawned once per session
// under the same gate as the UsageActor.
//
// Why it exists: the proxy records each forwarded request in a 256-entry
// in-process ring that dies with the daemon — so the only trail of governed
// traffic did not survive a restart, and design 013's org-audit forwarding had
// nothing durable to forward. This actor closes that gap by capturing every
// MsgProxyRequestAudit into a per-session JetStream stream (file-backed, so it
// outlives the process), and serving the ##proxy audit view from it.
//
// The actor does NOT subscribe to the audit subject itself: the JetStream
// stream is configured on that subject, so the broker captures every published
// record with no mailbox hop. The actor only (1) ensures the stream exists and
// (2) answers snapshot requests by reading the tail of the stream. That keeps
// the hot path a pure data-plane publish and the actor's job small.
//
// Degradation: if JetStream is unavailable the stream cannot be created; the
// actor then reports Durable=false and empty records, and ##proxy audit falls
// back to the proxy's in-process ring. This mirrors the UsageActor, which also
// degrades to in-memory when KV is unavailable.
type ProxyAuditActor struct {
	sessionName string
	pub         *msg.NATSPublisher
	nc          *nats.Conn
	br          *bridge.NATSBridge

	js         nats.JetStreamContext
	streamName string
	streamOK   bool
}

// Ring/stream sizing. The display ring bounds how many records ##proxy audit can
// return in one call; the stream itself retains far more for org forwarding.
const (
	proxyAuditDefaultLimit = 20
	proxyAuditMaxLimit     = 512
	proxyAuditStreamMaxMsg = 100_000
	proxyAuditRetention    = 90 * 24 * time.Hour
)

// NewProxyAuditActor constructs the audit-plane actor. The JetStream stream is
// opened lazily in the Started handler; if JetStream is unavailable the actor
// still serves (empty, non-durable) snapshots rather than failing to start.
func NewProxyAuditActor(sessionName string, pub *msg.NATSPublisher, nc *nats.Conn) *ProxyAuditActor {
	if sessionName == "" {
		sessionName = "default"
	}
	return &ProxyAuditActor{
		sessionName: sessionName,
		pub:         pub,
		nc:          nc,
	}
}

func (a *ProxyAuditActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		a.openStream()
		a.br = bridge.New(a.nc, ctx.Self(), ctx.ActorSystem(), a.pub.Codecs())
		if err := a.br.AddSubject(msg.ProxyAuditInboxSubject()); err != nil {
			slog.Error("proxy-audit: subscribe failed", "subject", msg.ProxyAuditInboxSubject(), "err", err)
		}

	case *actor.Stopping:
		if a.br != nil {
			a.br.Stop()
			a.br = nil
		}

	case *msg.RequestEnvelope:
		if req, ok := m.Inner.(*msg.MsgProxyAuditSnapshotRequest); ok {
			_ = m.Reply(a.snapshot(req.Limit))
		}
	}
}

// openStream ensures the per-session JetStream stream exists on the proxy-audit
// subject. Idempotent: on restart the stream already exists and is reused.
func (a *ProxyAuditActor) openStream() {
	if a.nc == nil {
		return
	}
	js, err := a.nc.JetStream()
	if err != nil {
		slog.Warn("proxy-audit: JetStream unavailable, audit trail non-durable", "err", err)
		return
	}
	a.js = js
	a.streamName = "rysh-proxy-audit-" + sanitizeStreamName(a.sessionName)
	cfg := &nats.StreamConfig{
		Name:      a.streamName,
		Subjects:  []string{msg.ProxyAuditWildcardSubject()},
		Storage:   nats.FileStorage,
		Retention: nats.LimitsPolicy,
		Discard:   nats.DiscardOld,
		MaxMsgs:   proxyAuditStreamMaxMsg,
		MaxAge:    proxyAuditRetention,
	}
	// Create if missing; reuse if present. AddStream errors when a stream of the
	// same name already exists, so check first.
	if _, err := js.StreamInfo(a.streamName); err != nil {
		if _, err := js.AddStream(cfg); err != nil {
			slog.Warn("proxy-audit: stream unavailable, audit trail non-durable",
				"stream", a.streamName, "err", err)
			return
		}
	}
	a.streamOK = true
	slog.Info("proxy-audit: durable trail active", "stream", a.streamName)
}

// snapshot returns up to `limit` most-recent audit records, oldest first, read
// from the tail of the durable stream.
func (a *ProxyAuditActor) snapshot(limit int) *msg.MsgProxyAuditSnapshotReply {
	if limit <= 0 {
		limit = proxyAuditDefaultLimit
	}
	if limit > proxyAuditMaxLimit {
		limit = proxyAuditMaxLimit
	}
	reply := &msg.MsgProxyAuditSnapshotReply{Durable: a.streamOK}
	if !a.streamOK {
		return reply
	}

	info, err := a.js.StreamInfo(a.streamName)
	if err != nil {
		slog.Warn("proxy-audit: stream info failed", "err", err)
		reply.Durable = false
		return reply
	}
	last := info.State.LastSeq
	if last == 0 {
		return reply // no records yet
	}
	first := info.State.FirstSeq
	start := uint64(1)
	if last >= uint64(limit) {
		start = last - uint64(limit) + 1
	}
	if start < first {
		start = first
	}

	for seq := start; seq <= last; seq++ {
		rm, err := a.js.GetMsg(a.streamName, seq)
		if err != nil {
			continue // purged or expired sequence — a hole in the tail, skip it
		}
		if rec, ok := a.decodeAudit(rm.Data); ok {
			reply.Records = append(reply.Records, rec)
		}
	}
	return reply
}

// decodeAudit unwraps a stored NATSEnvelope into a MsgProxyRequestAudit.
func (a *ProxyAuditActor) decodeAudit(data []byte) (msg.MsgProxyRequestAudit, bool) {
	var env msg.NATSEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return msg.MsgProxyRequestAudit{}, false
	}
	if env.TypeTag != msg.TagProxyRequestAudit {
		return msg.MsgProxyRequestAudit{}, false
	}
	var rec msg.MsgProxyRequestAudit
	if err := json.Unmarshal(env.Payload, &rec); err != nil {
		return msg.MsgProxyRequestAudit{}, false
	}
	return rec, true
}

// sanitizeStreamName keeps a session name to the character set JetStream allows
// in a stream name (no '.', '*', '>', whitespace).
func sanitizeStreamName(s string) string {
	if s == "" {
		return "default"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// StreamNameForSession is exposed for tests that assert against the stream.
func StreamNameForSession(session string) string {
	return "rysh-proxy-audit-" + sanitizeStreamName(session)
}
