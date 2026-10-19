// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/company"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// CompanyActor — the session's company follower (design 037, "Company
// Connect"). Root-spawned as the singleton "company" when the config carries a
// `company:` block. It keeps one snapshot of the bound AI-native company fresh
// (REST + live NATS hints + a poll), pushes it on company.snapshot, and answers
// company.query with the latest one so a TUI that attaches later does not wait
// for the next poll. company.refresh asks it to fetch now.
//
// It is a daemon-side actor for the same reason ABLA is: the data must be
// gathered whether or not a front-end is looking, so the desktop app and the
// terminal both attach to a session that already knows its company.
type CompanyActor struct {
	nc       *nats.Conn
	pub      *msg.NATSPublisher
	cfg      config.CompanyConfig
	upstream config.UpstreamConfig

	mu       sync.Mutex
	latest   msg.CompanySnapshot
	follower *company.Follower
	cancel   context.CancelFunc
	subs     []*nats.Subscription
}

func NewCompanyActor(nc *nats.Conn, pub *msg.NATSPublisher, cfg config.CompanyConfig, up config.UpstreamConfig) *CompanyActor {
	return &CompanyActor{nc: nc, pub: pub, cfg: cfg, upstream: up}
}

func (a *CompanyActor) Receive(ctx actor.Context) {
	switch ctx.Message().(type) {
	case *actor.Started:
		a.start()
	case *actor.Stopping:
		a.stop()
	}
}

func (a *CompanyActor) start() {
	if a.nc == nil || a.pub == nil || !a.cfg.Enabled() {
		return
	}
	server := a.cfg.Server
	if server == "" {
		server = a.upstream.URL
	}
	// The view has a name before the first fetch returns.
	a.mu.Lock()
	a.latest = msg.CompanySnapshot{Company: msg.CompanyInfo{
		ID: a.cfg.ID, Name: a.cfg.Name, Slug: a.cfg.Slug, FQDN: a.cfg.FQDN, ServerURL: server,
	}}
	a.mu.Unlock()

	client := company.NewClient(server, a.upstream.APIKey)
	a.follower = company.New(client, a.cfg.ID, a.upstream.WorkspaceName(), a.upstream, a.publish)
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel

	if sub, err := a.nc.Subscribe(msg.CompanyQuerySubject(), a.handleQuery); err == nil {
		a.subs = append(a.subs, sub)
	} else {
		slog.Warn("company: query subscription failed", "err", err)
	}
	if sub, err := a.nc.Subscribe(msg.CompanyRefreshSubject(), func(*nats.Msg) { a.follower.Refresh() }); err == nil {
		a.subs = append(a.subs, sub)
	}
	go a.follower.Run(ctx)
	slog.Info("company: following", "company", a.cfg.Slug, "server", server)
}

func (a *CompanyActor) stop() {
	if a.cancel != nil {
		a.cancel()
	}
	for _, s := range a.subs {
		_ = s.Unsubscribe()
	}
	a.subs = nil
}

// publish is the follower's callback: remember the snapshot and push it.
func (a *CompanyActor) publish(snap msg.CompanySnapshot) {
	a.mu.Lock()
	a.latest = snap
	a.mu.Unlock()
	if err := a.pub.Send(msg.CompanySnapshotSubject(), &msg.MsgCompanySnapshot{Snapshot: snap}); err != nil {
		slog.Warn("company: publish snapshot failed", "err", err)
	}
}

// handleQuery answers company.query. NATSPublisher.Request carries the reply
// inbox INSIDE the JSON envelope (NATSEnvelope.ReplyTo) and publishes with a
// plain Publish, so the NATS-level Reply header is empty; a raw subscriber
// must read the envelope to find where to answer.
func (a *CompanyActor) handleQuery(m *nats.Msg) {
	var env msg.NATSEnvelope
	_ = json.Unmarshal(m.Data, &env)
	reply := env.ReplyTo
	if reply == "" {
		reply = m.Reply
	}
	if reply == "" {
		return
	}
	a.mu.Lock()
	snap := a.latest
	a.mu.Unlock()
	data, err := encodeReply(a.pub.Codecs(), &msg.MsgCompanySnapshot{Snapshot: snap})
	if err != nil {
		slog.Warn("company: encode query reply failed", "err", err)
		return
	}
	if err := a.nc.Publish(reply, data); err != nil {
		slog.Warn("company: query reply failed", "err", err)
	}
}

// encodeReply wraps a registered message in the envelope NATSPublisher.Request
// decodes, for a reply sent from a raw NATS callback.
func encodeReply(codecs *msg.CodecRegistry, message any) ([]byte, error) {
	tag := codecs.TagOf(message)
	if tag == "" {
		return nil, fmt.Errorf("unregistered message type %T", message)
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	return json.Marshal(msg.NATSEnvelope{TypeTag: tag, Payload: payload})
}
