package actors

import (
	"log/slog"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ShareEntry tracks a single active share.
type ShareEntry struct {
	ShareID          string
	EntityType       string
	EntityID         string
	EntityAlias      string
	Mode             string
	ActorPID         *actor.PID
	Connected        bool
	PaneIDs          []string
	SharedRootFolder string
}

// ShareRegistryActor manages all active upstream shares for the session.
// It is spawned as a child of WorkspaceActor and coordinates share lifecycle.
type ShareRegistryActor struct {
	sessionName string
	config      config.UpstreamConfig
	pub         *msg.NATSPublisher
	nc          *nats.Conn
	br          *bridge.NATSBridge
	shares      map[string]*ShareEntry // entityID -> entry
	system      *actor.ActorSystem
	// forge runs forge-origin operations for control-mode forged-API shares
	// (Task 2 phase 2b). Passed down to each UpstreamShareActor. Nil when forge
	// is unavailable, in which case invoke_op requests are rejected.
	forge forgeOpRunner
	// subscribeWebInbox controls whether this registry subscribes to the
	// session-scoped share.registry.inbox subject (used by the web server for
	// status/list queries). With multiple workspaces each running their own
	// registry, only one may own that shared subject; the rest are driven
	// purely in-process by their WorkspaceActor.
	subscribeWebInbox bool
}

// NewShareRegistryActor creates a new ShareRegistryActor. subscribeWebInbox
// should be true for exactly one registry per session (the primary workspace).
func NewShareRegistryActor(
	sessionName string,
	cfg config.UpstreamConfig,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	system *actor.ActorSystem,
	subscribeWebInbox bool,
	forge forgeOpRunner,
) *ShareRegistryActor {
	return &ShareRegistryActor{
		sessionName:       sessionName,
		config:            cfg,
		pub:               pub,
		nc:                nc,
		shares:            make(map[string]*ShareEntry),
		system:            system,
		forge:             forge,
		subscribeWebInbox: subscribeWebInbox,
	}
}

// Receive implements actor.Actor.
func (s *ShareRegistryActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		s.br = bridge.New(s.nc, ctx.Self(), ctx.ActorSystem(), s.pub.Codecs())

		// Subscribe to the session-scoped share registry inbox (web-server
		// status/list queries) only for the primary workspace's registry, so
		// multiple per-workspace registries don't contend for the same subject.
		// Share/unshare commands still arrive in-process from the WorkspaceActor
		// regardless, so every workspace can share its own panes.
		if s.subscribeWebInbox {
			subject := msg.T("share", "registry", "inbox")
			if err := s.br.AddSubject(subject); err != nil {
				slog.Error("share-registry: subscribe failed",
					"subject", subject, "err", err)
			}
		}

		slog.Info("share-registry: started",
			"session", s.sessionName, "workspace", s.config.WorkspaceName(), "webInbox", s.subscribeWebInbox)

	case *actor.Stopping:
		// Stop all child share actors.
		for entityID, entry := range s.shares {
			if entry.ActorPID != nil {
				ctx.Stop(entry.ActorPID)
				slog.Info("share-registry: stopped child share actor",
					"entityID", entityID, "shareID", entry.ShareID)
			}
		}
		s.shares = make(map[string]*ShareEntry)

		if s.br != nil {
			s.br.Stop()
			s.br = nil
		}

		slog.Info("share-registry: stopped", "session", s.sessionName)

	case *msg.MsgShareEntity:
		// When sharing a single pane, include it in paneIDs so the
		// UpstreamShareActor subscribes to its shared output.
		var paneIDs []string
		if m.EntityType == "pane" {
			paneIDs = []string{m.EntityID}
		}
		// Prefer the human-readable alias (e.g. "<given> · <auto>" for panes) so
		// remote viewers can identify the entity; fall back to the id.
		alias := m.EntityAlias
		if alias == "" {
			alias = m.EntityID
		}
		s.startShare(ctx, m.EntityType, m.EntityID, alias, m.Mode, m.ShareID, paneIDs, m.SharedRootFolder, m.ShareAPI, m.ForgedOps, m.Redact)

	case *msg.MsgUnshareEntity:
		s.stopShare(ctx, m.EntityID)

	// Direct proto.actor request/respond (from workspace via RequestFuture).
	case *msg.MsgShareList:
		infos := s.getShareInfos()
		ctx.Respond(&msg.MsgShareListReply{Shares: infos})

	case *msg.MsgShareStatus:
		var infos []msg.ShareInfo
		if m.EntityID != "" {
			if entry, ok := s.shares[m.EntityID]; ok {
				infos = append(infos, msg.ShareInfo{
					ShareID:    entry.ShareID,
					EntityType: entry.EntityType,
					EntityID:   entry.EntityID,
					Alias:      entry.EntityAlias,
					Mode:       entry.Mode,
					Connected:  entry.Connected,
					URL:        s.config.URL,
				})
			}
		} else {
			infos = s.getShareInfos()
		}
		ctx.Respond(&msg.MsgShareStatusReply{Shares: infos})

	// NATS request/reply path (from NATSBridge).
	case *msg.RequestEnvelope:
		switch inner := m.Inner.(type) {
		case *msg.MsgShareStatus:
			var infos []msg.ShareInfo
			if inner.EntityID != "" {
				// Return status for a specific entity.
				if entry, ok := s.shares[inner.EntityID]; ok {
					infos = append(infos, msg.ShareInfo{
						ShareID:    entry.ShareID,
						EntityType: entry.EntityType,
						EntityID:   entry.EntityID,
						Alias:      entry.EntityAlias,
						Mode:       entry.Mode,
						Connected:  entry.Connected,
						URL:        s.config.URL,
					})
				}
			} else {
				// Return status for all shares.
				infos = s.getShareInfos()
			}
			_ = m.Reply(&msg.MsgShareStatusReply{Shares: infos})

		case *msg.MsgShareList:
			_ = inner // suppress unused
			infos := s.getShareInfos()
			_ = m.Reply(&msg.MsgShareListReply{Shares: infos})
		}
	}
}

// startShare spawns a new UpstreamShareActor as a child and tracks it.
func (s *ShareRegistryActor) startShare(ctx actor.Context, entityType, entityID, entityAlias, mode, preShareID string, paneIDs []string, sharedRootFolder string, shareAPI bool, forgedOps []msg.ForgedOpSpec, redact bool) {
	// Check if already shared.
	if _, exists := s.shares[entityID]; exists {
		slog.Warn("share-registry: entity already shared",
			"entityID", entityID, "entityType", entityType)
		return
	}

	shareID := preShareID
	if shareID == "" {
		shareID = uuid.NewString()
	}

	props := actor.PropsFromProducer(func() actor.Actor {
		return NewUpstreamShareActor(shareID, entityType, entityID, entityAlias, mode, s.sessionName, s.config, s.pub, s.nc, paneIDs, sharedRootFolder, shareAPI, forgedOps, redact, s.forge)
	})
	pid := ctx.Spawn(props)

	entry := &ShareEntry{
		ShareID:          shareID,
		EntityType:       entityType,
		EntityID:         entityID,
		EntityAlias:      entityAlias,
		Mode:             mode,
		ActorPID:         pid,
		Connected:        true,
		PaneIDs:          paneIDs,
		SharedRootFolder: sharedRootFolder,
	}
	s.shares[entityID] = entry

	slog.Info("share-registry: share started",
		"shareID", shareID, "entityType", entityType,
		"entityID", entityID, "mode", mode)
}

// stopShare stops a child share actor and removes it from the registry.
func (s *ShareRegistryActor) stopShare(ctx actor.Context, entityID string) {
	entry, ok := s.shares[entityID]
	if !ok {
		slog.Warn("share-registry: entity not found for unshare",
			"entityID", entityID)
		return
	}

	if entry.ActorPID != nil {
		ctx.Stop(entry.ActorPID)
	}

	delete(s.shares, entityID)

	slog.Info("share-registry: share stopped",
		"shareID", entry.ShareID, "entityID", entityID)
}

// getShareInfos returns the current state of all shares as a ShareInfo slice.
func (s *ShareRegistryActor) getShareInfos() []msg.ShareInfo {
	infos := make([]msg.ShareInfo, 0, len(s.shares))
	for _, entry := range s.shares {
		infos = append(infos, msg.ShareInfo{
			ShareID:    entry.ShareID,
			EntityType: entry.EntityType,
			EntityID:   entry.EntityID,
			Alias:      entry.EntityAlias,
			Mode:       entry.Mode,
			Connected:  entry.Connected,
			URL:        s.config.URL,
		})
	}
	return infos
}
