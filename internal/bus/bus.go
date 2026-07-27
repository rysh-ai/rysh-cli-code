package bus

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

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
	objStore   nats.ObjectStore // "rysh-images" — large conversation images (follow-up 1b)
	system     *actor.ActorSystem
	codecs     *msg.CodecRegistry
	pub        *msg.NATSPublisher
	clientPort int // TCP port for external CLI clients (0 if not listening)
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
		existingURL := fmt.Sprintf("nats://127.0.0.1:%d", cliPort)
		nc, err := nats.Connect(existingURL,
			nats.MaxReconnects(0),
			nats.Timeout(1*time.Second),
		)
		if err == nil {
			// An existing NATS server is running — reuse it.
			b.nc = nc
			b.clientPort = cliPort
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
	pKV, err := js.CreateKeyValue(&nats.KeyValueConfig{
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
	if ostore, oerr := js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: imagesBucket, Storage: nats.FileStorage}); oerr == nil {
		b.objStore = ostore
		b.pKV = newImageOffloadKV(pKV, ostore)
	} else if ostore, oerr2 := js.ObjectStore(imagesBucket); oerr2 == nil {
		b.objStore = ostore
		b.pKV = newImageOffloadKV(pKV, ostore)
	}

	wKV, err := js.CreateKeyValue(&nats.KeyValueConfig{
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

	plKV, err := js.CreateKeyValue(&nats.KeyValueConfig{
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

	aKV, err := js.CreateKeyValue(&nats.KeyValueConfig{
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
	if sKV, serr := js.CreateKeyValue(&nats.KeyValueConfig{
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
	if vKV, verr := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:  variableBucket,
		Storage: nats.FileStorage,
	}); verr == nil {
		b.variableKV = vKV
	} else if vKV, verr2 := js.KeyValue(variableBucket); verr2 == nil {
		b.variableKV = vKV
	}

	// Create proto.actor ActorSystem with a silent logger to prevent
	// "actor system started" from printing to stderr.
	b.system = actor.NewActorSystem(actor.WithLoggerFactory(func(_ *actor.ActorSystem) *slog.Logger {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}))

	// Create codec registry and publisher.
	b.codecs = msg.DefaultCodecRegistry()
	b.pub = msg.NewNATSPublisher(b.nc, b.codecs)

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

// ActorSystem returns the proto.actor ActorSystem.
func (b *Bus) ActorSystem() *actor.ActorSystem { return b.system }

// Publisher returns the NATSPublisher for sending typed messages.
func (b *Bus) Publisher() *msg.NATSPublisher { return b.pub }

// Codecs returns the CodecRegistry.
func (b *Bus) Codecs() *msg.CodecRegistry { return b.codecs }

// ClientPort returns the TCP port external CLI clients can connect to.
// Returns 0 if no TCP listener is available (e.g. external NATS mode).
func (b *Bus) ClientPort() int { return b.clientPort }

// freePort asks the OS for an available TCP port on localhost.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
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
