// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Config holds the settings needed to initialise a Bus.
type Config struct {
	Mode        string // "embedded" or "external"
	URL         string // NATS URL, used when Mode == "external"
	DataDir     string // JetStream storage directory (empty = auto)
	SessionName string // used to isolate data dirs per session
	Port        int    // TCP port for embedded NATS server (0 = auto)
}

// Bus wraps either an embedded NATS server or an external connection.
// It exposes a plain *nats.Conn, two JetStream KV buckets, a proto.actor
// ActorSystem, a CodecRegistry, and a NATSPublisher.
type Bus struct {
	ns         *server.Server // nil when using an external server
	nc         *nats.Conn
	js         nats.JetStreamContext
	pKV        nats.KeyValue    // "rysh-panes" (image-offloading wrapper when objStore != nil)
	wKV        nats.KeyValue    // "rysh-workspace"
	plKV       nats.KeyValue    // "rysh-pipeline"
	aKV        nats.KeyValue    // "rysh-agents"
	secretKV   nats.KeyValue    // "rysh-secrets" — per-session secret store
	variableKV nats.KeyValue    // "rysh-variables" — per-session env-variable store
	boardKV    nats.KeyValue    // "rysh-board" — per-session agents-board history
	fleetKV    nats.KeyValue    // "rysh-fleet" — per-session fleet registry (design 028)
	objStore   nats.ObjectStore // "rysh-images" — large conversation images (follow-up 1b)
	system     *actor.ActorSystem
	codecs     *msg.CodecRegistry
	pub        *msg.NATSPublisher
	clientPort int // TCP port for external CLI clients (0 if not listening)

	// guest is true when this Bus connected to a NATS server started by some
	// other process (another session's daemon, or the desktop app's sidecar)
	// rather than starting its own. A guest outlives its server, so it is the
	// only case that has to survive that server going away and coming back.
	guest bool
	// kvConfigs are the bucket declarations this Bus depends on, recorded as
	// they are created so redeclareBuckets can re-declare them. See ensureKV.
	kvConfigs []*nats.KeyValueConfig
	objConfig *nats.ObjectStoreConfig
}

// New creates and starts a Bus according to cfg.
func New(cfg Config) (*Bus, error) {
	b := &Bus{}

	switch cfg.Mode {
	case "external":
		nc, err := nats.Connect(cfg.URL,
			nats.MaxReconnects(-1),
			nats.ReconnectWait(500*time.Millisecond),
		)
		if err != nil {
			return nil, fmt.Errorf("connect to external NATS at %s: %w", cfg.URL, err)
		}
		b.nc = nc

	default: // "embedded"
		cliPort := cfg.Port
		if cliPort <= 0 {
			cliPort = 24242
		}

		// Try connecting to an existing NATS server on the configured port.
		//
		// Timeout(1s) is what keeps this a probe: a dead port fails at once
		// (RetryOnFailedConnect defaults to false, so the *initial* dial is
		// never retried whatever MaxReconnects says) and we fall through to
		// starting our own server below.
		//
		// MaxReconnects governs only what happens after a successful connect,
		// and for this branch it has to be unlimited. Connecting here makes us
		// a GUEST on a server we do not own — usually the desktop app's
		// sidecar — and that server gets restarted out from under us as a
		// matter of routine: `make restart` in rysh-cli-app pkill -9's the
		// sidecar, taking its embedded NATS with it. This used to be
		// MaxReconnects(0), which strands the guest permanently the first time
		// that happens: the daemon keeps its pane processes, keeps serving its
		// web viewer and keeps writing "state": "running" to its session file,
		// while every ## command against it times out on <session>.ws.inbox
		// forever. Alive, addressable-looking, and off the bus.
		existingURL := fmt.Sprintf("nats://127.0.0.1:%d", cliPort)
		nc, err := nats.Connect(existingURL,
			nats.MaxReconnects(-1),
			nats.ReconnectWait(500*time.Millisecond),
			nats.Timeout(1*time.Second),
		)
		if err == nil {
			// An existing NATS server is running — reuse it.
			b.nc = nc
			b.clientPort = cliPort
			b.guest = true
		} else {
			// No server running — start an embedded one. rysh state is always
			// project-local: when no data dir was configured, fall back to
			// "<cwd>/.rysh/nats" (never a global ~/.local/state/rysh).
			dataDir := cfg.DataDir
			if dataDir == "" {
				cwd, err := os.Getwd()
				if err != nil {
					return nil, fmt.Errorf("determine working dir: %w", err)
				}
				dataDir = filepath.Join(cwd, ".rysh", "nats")
			}
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				return nil, fmt.Errorf("create NATS data dir: %w", err)
			}

			opts := &server.Options{
				Host:      "127.0.0.1",
				Port:      cliPort,
				JetStream: true,
				StoreDir:  dataDir,
				NoLog:     true,
				NoSigs:    true,
				// Default NATS max payload is 1MB, which a single browser-pane
				// screenshot (base64 JPEG, esp. on a Retina display) can exceed —
				// causing the browser_action response to be dropped silently and
				// the tool to time out. Raise it generously.
				MaxPayload: 32 * 1024 * 1024,
			}
			ns, err := server.NewServer(opts)
			if err != nil {
				return nil, fmt.Errorf("create embedded NATS server: %w", err)
			}
			ns.Start()
			if !ns.ReadyForConnections(5 * time.Second) {
				ns.Shutdown()
				return nil, fmt.Errorf("embedded NATS server failed to become ready")
			}
			b.ns = ns
			b.clientPort = cliPort

			nc, err := nats.Connect(nats.DefaultURL, nats.InProcessServer(ns))
			if err != nil {
				ns.Shutdown()
				return nil, fmt.Errorf("connect to embedded NATS: %w", err)
			}
			b.nc = nc
		}
	}

	// Obtain JetStream context.
	js, err := b.nc.JetStream()
	if err != nil {
		b.nc.Close()
		if b.ns != nil {
			b.ns.Shutdown()
		}
		return nil, fmt.Errorf("get JetStream context: %w", err)
	}
	b.js = js

	// Namespace KV buckets by session so multiple sessions share one NATS server.
	sessionSuffix := cfg.SessionName
	if sessionSuffix == "" {
		sessionSuffix = "default"
	}
	paneBucket := fmt.Sprintf("rysh-panes-%s", sessionSuffix)
	wsBucket := fmt.Sprintf("rysh-workspace-%s", sessionSuffix)
	plBucket := fmt.Sprintf("rysh-pipeline-%s", sessionSuffix)
	agentBucket := fmt.Sprintf("rysh-agents-%s", sessionSuffix)
	secretBucket := fmt.Sprintf("rysh-secrets-%s", sessionSuffix)
	variableBucket := fmt.Sprintf("rysh-variables-%s", sessionSuffix)

	// Create KV buckets (idempotent — existing buckets are reused).
	pKV, err := b.ensureKV(&nats.KeyValueConfig{
		Bucket:  paneBucket,
		Storage: nats.FileStorage,
	})
	if err != nil {
		b.nc.Close()
		if b.ns != nil {
			b.ns.Shutdown()
		}
		return nil, fmt.Errorf("create %s KV: %w", paneBucket, err)
	}
	b.pKV = pKV

	// Follow-up 1b: offload large inline images in conversation values to a
	// JetStream Object Store. A single >~768 KB image base64-encodes past the
	// default 1 MB per-message limit, so the conversation Put would otherwise
	// fail silently (the persist path ignores the error) and the conversation
	// would be lost on restart. The wrapper transparently swaps inline base64
	// images for Object Store refs on Put and rehydrates them on Get; all other
	// keys pass through. Falls back to the raw KV (inline) if the Object Store
	// can't be created (e.g. an older server).
	imagesBucket := fmt.Sprintf("rysh-images-%s", sessionSuffix)
	if ostore, oerr := b.ensureObjectStore(&nats.ObjectStoreConfig{Bucket: imagesBucket, Storage: nats.FileStorage}); oerr == nil {
		b.objStore = ostore
		b.pKV = newImageOffloadKV(pKV, ostore)
	} else if ostore, oerr2 := js.ObjectStore(imagesBucket); oerr2 == nil {
		b.objStore = ostore
		b.pKV = newImageOffloadKV(pKV, ostore)
	}

	wKV, err := b.ensureKV(&nats.KeyValueConfig{
		Bucket:  wsBucket,
		Storage: nats.FileStorage,
	})
	if err != nil {
		b.nc.Close()
		if b.ns != nil {
			b.ns.Shutdown()
		}
		return nil, fmt.Errorf("create %s KV: %w", wsBucket, err)
	}
	b.wKV = wKV

	plKV, err := b.ensureKV(&nats.KeyValueConfig{
		Bucket:  plBucket,
		Storage: nats.FileStorage,
	})
	if err != nil {
		// try KeyValue() as fallback (bucket may already exist)
		plKV, err = js.KeyValue(plBucket)
		if err != nil {
			return nil, fmt.Errorf("pipeline KV: %w", err)
		}
	}
	b.plKV = plKV

	aKV, err := b.ensureKV(&nats.KeyValueConfig{
		Bucket:  agentBucket,
		Storage: nats.FileStorage,
	})
	if err != nil {
		// try KeyValue() as fallback (bucket may already exist)
		aKV, err = js.KeyValue(agentBucket)
		if err != nil {
			return nil, fmt.Errorf("agent KV: %w", err)
		}
	}
	b.aKV = aKV

	// Per-session secret store. Failure here is non-fatal: the secret store
	// degrades to config + environment resolution when no bucket is available.
	if sKV, serr := b.ensureKV(&nats.KeyValueConfig{
		Bucket:  secretBucket,
		Storage: nats.FileStorage,
	}); serr == nil {
		b.secretKV = sKV
	} else if sKV, serr2 := js.KeyValue(secretBucket); serr2 == nil {
		b.secretKV = sKV
	}

	// Per-session variable store (##variable). Same manner as the secret store,
	// stored in its own bucket; failure here is non-fatal, degrading to config +
	// environment resolution when no bucket is available.
	if vKV, verr := b.ensureKV(&nats.KeyValueConfig{
		Bucket:  variableBucket,
		Storage: nats.FileStorage,
	}); verr == nil {
		b.variableKV = vKV
	} else if vKV, verr2 := js.KeyValue(variableBucket); verr2 == nil {
		b.variableKV = vKV
	}

	// Per-session agents-board history (design 025; founder gate 2 — the board
	// survives a daemon restart). Same manner as the two stores above: failure
	// is non-fatal and degrades the board to live-only rather than blocking a
	// session from starting over a monitoring view. The bucket's retention
	// (TTL + MaxBytes) is declared in internal/board beside the comment that
	// documents the numbers, so the two cannot drift apart.
	if bKV, berr := b.ensureKV(board.BucketConfig(sessionSuffix)); berr == nil {
		b.boardKV = bKV
	} else if bKV, berr2 := js.KeyValue(board.BucketName(sessionSuffix)); berr2 == nil {
		b.boardKV = bKV
	}

	// Per-session fleet registry (design 028 §6.5). Same manner and the same
	// non-fatal failure as the board's bucket above: a session must start even
	// when a coordination surface cannot be persisted, and the registry then
	// degrades to one that forgets on restart rather than blocking a start.
	if fKV, ferr := b.ensureKV(fleet.BucketConfig(sessionSuffix)); ferr == nil {
		b.fleetKV = fKV
	} else if fKV, ferr2 := js.KeyValue(fleet.BucketName(sessionSuffix)); ferr2 == nil {
		b.fleetKV = fKV
	}

	// Create proto.actor ActorSystem with a silent logger to prevent
	// "actor system started" from printing to stderr.
	b.system = actor.NewActorSystem(actor.WithLoggerFactory(func(_ *actor.ActorSystem) *slog.Logger {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}))

	// Create codec registry and publisher.
	b.codecs = msg.DefaultCodecRegistry()
	b.pub = msg.NewNATSPublisher(b.nc, b.codecs)

	// Only a guest can lose its server while it is still running: an embedded
	// server dies with this process, and the external-mode connection already
	// reconnects forever.
	if b.guest {
		b.watchConnection()
	}

	return b, nil
}

// Conn returns the underlying NATS connection.
func (b *Bus) Conn() *nats.Conn { return b.nc }

// JS returns the JetStream context.
func (b *Bus) JS() nats.JetStreamContext { return b.js }

// PaneKV returns the KV bucket for pane snapshots.
func (b *Bus) PaneKV() nats.KeyValue { return b.pKV }

// WorkspaceKV returns the KV bucket for workspace state.
func (b *Bus) WorkspaceKV() nats.KeyValue { return b.wKV }

// PipelineKV returns the KV bucket for pipeline prompt persistence.
func (b *Bus) PipelineKV() nats.KeyValue { return b.plKV }

// AgentKV returns the KV bucket for agent/humanoid persistence.
func (b *Bus) AgentKV() nats.KeyValue { return b.aKV }

// SecretKV returns the per-session secret store bucket. It may be nil when
// JetStream is unavailable, in which case secret resolution falls back to
// config + environment only.
func (b *Bus) SecretKV() nats.KeyValue { return b.secretKV }

// VariableKV returns the per-session variable store bucket. It may be nil when
// JetStream is unavailable, in which case variable resolution falls back to
// config + environment only.
func (b *Bus) VariableKV() nats.KeyValue { return b.variableKV }

// BoardKV returns the per-session agents-board history bucket. It may be nil
// when JetStream is unavailable, in which case the board is live-only: posts
// are delivered and rendered but do not survive a restart.
func (b *Bus) BoardKV() nats.KeyValue { return b.boardKV }

// FleetKV returns the per-session fleet registry bucket (design 028).
func (b *Bus) FleetKV() nats.KeyValue { return b.fleetKV }

// ActorSystem returns the proto.actor ActorSystem.
func (b *Bus) ActorSystem() *actor.ActorSystem { return b.system }

// Publisher returns the NATSPublisher for sending typed messages.
func (b *Bus) Publisher() *msg.NATSPublisher { return b.pub }

// Codecs returns the CodecRegistry.
func (b *Bus) Codecs() *msg.CodecRegistry { return b.codecs }

// ClientPort returns the TCP port external CLI clients can connect to.
// Returns 0 if no TCP listener is available (e.g. external NATS mode).
func (b *Bus) ClientPort() int { return b.clientPort }

// ensureKV declares a KV bucket and records the declaration. Creation is
// idempotent — an existing bucket is reused — and the record is what lets a
// guest Bus survive its host server restarting with an empty JetStream store,
// which is the normal outcome of rysh-cli-app's `make restart`.
//
// Re-declaring is enough because nats.go's KV handles are name-based: kvs holds
// the bucket name and the JetStream context, never any server-side handle. So
// recreating the streams after a reconnect revives every handle this Bus has
// already handed out — no field to swap, no lock to take, no caller to notify.
//
// Any bucket added to New must go through here, or it silently becomes the one
// bucket that does not come back.
func (b *Bus) ensureKV(cfg *nats.KeyValueConfig) (nats.KeyValue, error) {
	b.kvConfigs = append(b.kvConfigs, cfg)
	return b.js.CreateKeyValue(cfg)
}

// ensureObjectStore is ensureKV's counterpart for the images object store.
func (b *Bus) ensureObjectStore(cfg *nats.ObjectStoreConfig) (nats.ObjectStore, error) {
	b.objConfig = cfg
	return b.js.CreateObjectStore(cfg)
}

// watchConnection wires the connection callbacks for a guest Bus — one that
// borrowed another process's NATS server and therefore has to outlive it.
//
// It is installed at the end of New, after every ensureKV call has appended to
// kvConfigs, which is what makes the slice safe to read from the callback
// goroutine without a lock: the appends happen-before the handler exists.
func (b *Bus) watchConnection() {
	b.nc.SetDisconnectErrHandler(func(_ *nats.Conn, err error) {
		slog.Warn("bus: lost the NATS server, reconnecting", "err", err)
	})
	b.nc.SetReconnectHandler(func(nc *nats.Conn) {
		slog.Info("bus: reconnected to NATS", "url", nc.ConnectedUrl())
		// Off the callback goroutine: these are JetStream round-trips and
		// nats.go runs all connection callbacks on one queue.
		go b.redeclareBuckets()
	})
	b.nc.SetClosedHandler(func(_ *nats.Conn) {
		slog.Error("bus: NATS connection closed for good — this session is off the bus")
	})
}

// redeclareBuckets re-creates the buckets this Bus depends on after a
// reconnect. A server that merely bounced still has them and every call is a
// no-op; a server that came back with a fresh store does not, and without this
// the session reconnects into a half-working state where commands are answered
// but nothing persists.
func (b *Bus) redeclareBuckets() {
	for _, cfg := range b.kvConfigs {
		// Already there under a config we would not have written is still
		// "there", which is all this needs to establish.
		if _, err := b.js.CreateKeyValue(cfg); err != nil && !errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			slog.Warn("bus: could not re-declare KV bucket after reconnect", "bucket", cfg.Bucket, "err", err)
		}
	}
	if b.objConfig != nil && b.objStore != nil {
		if _, err := b.js.CreateObjectStore(b.objConfig); err != nil && !errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			slog.Warn("bus: could not re-declare object store after reconnect", "bucket", b.objConfig.Bucket, "err", err)
		}
	}
}

// Close shuts down the ActorSystem, drains the connection, and shuts down
// the embedded server (if any).
func (b *Bus) Close() {
	if b.system != nil {
		b.system.Shutdown()
	}
	if b.nc != nil {
		_ = b.nc.Drain()
	}
	if b.ns != nil {
		b.ns.Shutdown()
		b.ns.WaitForShutdown()
	}
}
