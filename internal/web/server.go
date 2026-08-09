// Package web provides an embedded web server that mirrors the rysh TUI in a
// browser. It connects to the same NATS bus as the TUI, polls workspace
// snapshots, and pushes them to WebSocket clients. Commands from the browser
// are published back to NATS so the actor hierarchy processes them identically.
package web

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

//go:embed static/*
var staticFiles embed.FS

// Server is the web UI server. It serves static assets over HTTP and maintains
// WebSocket connections to push real-time workspace snapshots to browsers.
type Server struct {
	port        int
	host        string // optional bind IP (empty = historical default; see listenAddr)
	sessionName string
	pub         *msg.NATSPublisher
	nc          *nats.Conn
	codecs      *msg.CodecRegistry
	logger      *slog.Logger

	// nextClientSeq numbers WebSocket connections so each gets a distinct id
	// for pane size arbitration (see handlePaneResize). Two app windows on the
	// same pane must be distinguishable, or the second would silently replace
	// the first's claim instead of adding to it.
	nextClientSeq atomic.Int64

	// creds, when non-nil, gates routes carrying session data behind a
	// username/password login (POST /api/auth/login) that mints a one-month JWT
	// the browser keeps in localStorage. Set with `##rysh web auth` or
	// `##rysh web start --username/--password`; see credentials.go. Guarded by
	// credsMu because credentials can be changed while the server is running.
	//
	// WHICH DOOR a request came through decides whether the gate applies at all
	// — see authMiddleware and StartShared.
	//
	// credsDir, when set, is the workspace rysh dir holding web-auth.json, and
	// the file — not this snapshot — becomes the source of truth. Every daemon
	// rooted at a workspace shares that one file (rysh state is project-local;
	// there is no ~/.rysh), so a second daemon running `##rysh web auth` mints a
	// new signing key that this process would otherwise never see. See
	// refreshCredentials and F-9.
	credsMu   sync.RWMutex
	creds     *Credentials
	credsDir  string
	credsStat credsFileStamp

	// fsBrowse serves the web UI's file browser (GET /fs/list, /fs/read).
	// Injected by the WorkspaceActor via SetFSBrowser — the implementation
	// lives in the actors package (sandbox rooted at the pane's cwd), which
	// this package cannot import. nil ⇒ endpoints answer "disabled".
	fsBrowse func(op, paneID, path string, offset, length int64) (any, string, string)

	// --- electronAPI parity (web_electron_roadmap Phase 3, W7–W10 + W12) ---
	// voice is the daemon's [voice]/[voice_control] config (SetVoice); the API
	// key never leaves the server. bashCompletion mirrors [ui]
	// shell_bash_completion (SetBashCompletion). wsPath/wsName identify the
	// current workspace (SetWorkspaceInfo); workspaceLister supplies recent
	// workspaces from the session registry (SetWorkspaceLister). transcribeFn
	// is the transcription entrypoint, overridable in tests (nil ⇒ the real
	// internal/voice provider call).
	voice           VoiceSettings
	bashCompletion  bool
	wsPath          string
	wsName          string
	workspaceLister func() []WorkspaceEntry
	transcribeFn    func(ctx context.Context, v VoiceSettings, audioPath string) (string, error)

	// webPanes holds the server-side embedded browsers for web-mode web panes
	// (W12, webpane.go), keyed by pane ID.
	webPaneMu sync.Mutex
	webPanes  map[string]*webPaneSession

	httpSrv          *http.Server

	// engine is the built route set, kept after Start so a SHARED listener can
	// be attached to the same routes, the same hub and the same session.
	engine http.Handler
	// sharedSrv/sharedLn are the second door: another listener onto those same
	// routes, on which a login is ALWAYS required. It exists so the desktop app
	// can keep its unauthenticated loopback connection while the same UI is
	// handed to a browser (or a phone) behind a password. nil ⇒ no shared door.
	sharedSrv  *http.Server
	sharedLn   net.Listener
	sharedHost string
	sharedPort int
	hub              *Hub
	cancel           context.CancelFunc
	mu               sync.Mutex
	running          bool
	approSub         *nats.Subscription
	browserSub       *nats.Subscription
	webPromptSub     *nats.Subscription
	webActivateSub   *nats.Subscription
	webDeactivateSub *nats.Subscription
	importCookiesSub *nats.Subscription
	pipelineSub      *nats.Subscription
	layoutSub        *nats.Subscription
	paneOutSubs      []*nats.Subscription
	emailListSub     *nats.Subscription
	emailDetailSub   *nats.Subscription
	emailChangedSub  *nats.Subscription
	waListSub        *nats.Subscription
	waDetailSub      *nats.Subscription
	waChangedSub     *nats.Subscription
	pairingSub       *nats.Subscription

	// control enables the WS5 control-dashboard surface (design 005 DB4):
	// pairing approve/allow, channel start/stop, governance and reply-mode
	// endpoints. Defaults from the RYSH_WEB_CONTROL env var (see
	// controlFromEnv); override with SetControl BEFORE Start. When control is
	// enabled the HTTP listener is forced onto 127.0.0.1 (loopback only).
	control bool

	// onClientCount, when set BEFORE Start, is invoked with the new total
	// WebSocket-client count on every connect/disconnect. The daemon wires it
	// to the session registry so app sessions can show "attached (app)".
	onClientCount func(n int)
}

// SetClientCountListener registers fn to receive the live WebSocket-client
// count on every transition. Must be called before Start.
func (s *Server) SetClientCountListener(fn func(n int)) {
	s.onClientCount = fn
}

// NewServer creates a new web UI server.
func NewServer(port int, sessionName string, pub *msg.NATSPublisher, nc *nats.Conn, codecs *msg.CodecRegistry) *Server {
	return &Server{
		port:        port,
		sessionName: sessionName,
		pub:         pub,
		nc:          nc,
		codecs:      codecs,
		logger:      slog.Default(),
		// Control mode (design 005 DB4) is opt-in via RYSH_WEB_CONTROL so the
		// existing WorkspaceActor call sites need no wiring changes: the default
		// `rysh web` surface stays the read-only-ish viewer, and control-plane
		// endpoints 403 until the env var (or SetControl) enables them.
		control: controlFromEnv(),
	}
}

// DefaultHost is the bind address used when none is configured: loopback, so
// a rysh web session is reachable only from the machine running the daemon
// unless the operator explicitly opts into a wider bind (`--bind 0.0.0.0`,
// [web] host, RYSH_WEB_HOST). The login is a guard, not a perimeter — binding
// the default surface to a LAN-visible interface is a decision the operator
// must make deliberately.
const DefaultHost = "127.0.0.1"

// SetHost pins the HTTP listener to a specific bind IP — 0.0.0.0 to expose the
// UI on every interface, a dedicated loopback alias like 127.0.100.1 fronted by
// a reverse proxy, and so on. Empty selects DefaultHost (loopback). Must be
// called before Start.
func (s *Server) SetHost(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.host = strings.TrimSpace(host)
}

// Host returns the configured bind IP ("" = default).
func (s *Server) Host() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.host
}

// Start launches the web server in background goroutines.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("web server already running on port %d", s.port)
	}
	s.mu.Unlock()

	// Bind synchronously so a taken port or unusable bind address fails HERE,
	// where the caller can report it — not in a background goroutine after
	// success (and a token URL) has already been printed.
	addr := s.listenAddr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", addr, err)
	}

	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	s.hub = newHub()
	s.hub.onCount = s.onClientCount
	go s.hub.run(ctx)

	// Start snapshot polling.
	go s.pollSnapshots(ctx)

	// Subscribe to approval requests and forward them to WebSocket clients.
	s.subscribeApprovalRequests()

	// Subscribe to browser-action requests (from the AI's browser_action tool)
	// and forward them to WebSocket clients (the web pane executes them).
	s.subscribeBrowserRequests()

	// Subscribe to web-AI prompt notices (`##mode web ai <prompt>` dispatched
	// from outside the chat box) and forward them so the app renders the prompt
	// as a human bubble in the Ask Rysh panel.
	s.subscribeWebPrompts()

	// Subscribe to web-activate notices (`##mode new web`) and forward them so
	// the app switches the pane to web display mode immediately.
	s.subscribeWebActivate()

	// Subscribe to web-deactivate notices (`##mode delete web`) and forward them
	// so the app drops the pane off web display mode immediately.
	s.subscribeWebDeactivate()

	// Subscribe to cookie-import notices (`##web import-google-session`) and
	// forward them so the app writes the cookies into the profile's session.
	s.subscribeImportCookies()

	// Subscribe to email-client events (inbox listing, single-email detail,
	// inbox-changed) published by email humanoids and forward them to the app.
	s.subscribeEmailEvents()
	s.subscribeWhatsAppEvents()

	// Subscribe to contact-pairing events (WS3 / design 005 DB2) published by
	// humanoids on humanoid.{name}.pairing and forward them to the dashboard.
	s.subscribePairingEvents()

	// Subscribe to pipeline output and forward to WebSocket clients.
	s.subscribePipelineOutput()

	// Content plane (delivered only to ?stream=1 clients): event-driven
	// layout-only snapshots on ws.layoutDirty + per-pane content deltas + a fast
	// pull of interactive panes' VT (which the layout-only snapshot omits).
	s.subscribeLayoutDirty(ctx)
	s.subscribePaneOutput()
	go s.streamPaneVT(ctx)

	// Setup Gin in release mode (no debug logs).
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	// Login gate (no-op when no credentials are configured). Must precede routes
	// so session data, and the /ws upgrade, are all protected.
	r.Use(s.authMiddleware())

	// Serve embedded static files.
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		cancel()
		return fmt.Errorf("embed static fs: %w", err)
	}

	// index.html backs BOTH the desktop UI ("/") and the mobile drill-down UI
	// ("/mobile[/]"). It is the SAME single-page app — main.tsx branches on
	// location.pathname — and it loads its JS/CSS from absolute /assets/… paths,
	// so one embedded file serves either route.
	serveIndex := func(c *gin.Context) {
		data, err := staticFiles.ReadFile("static/index.html")
		if err != nil {
			c.String(http.StatusInternalServerError, "failed to load index.html")
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", data)
	}
	r.GET("/", serveIndex)
	// Mobile-optimized UI (small screens): a tabs → panes → single-pane
	// drill-down instead of the desktop column/stack layout.
	r.GET("/mobile", serveIndex)
	r.GET("/mobile/", serveIndex)
	// Serve Vite-built assets at /assets/ (JS, CSS bundles).
	r.StaticFS("/assets", http.FS(newPrefixFS(staticFS, "assets")))
	// Keep /static for backward compatibility.
	r.StaticFS("/static", http.FS(staticFS))
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	// Username/password login (auth.go). Reachable without credentials — it is
	// how a browser acquires them.
	s.registerAuthAPI(r)
	r.GET("/ws", s.handleWebSocket)
	// File browser for the web UI (mobile drill-down's file viewer): list a
	// directory / read a text-or-image file, sandboxed to the pane's cwd.
	// Same reply shapes as the share relay's fs endpoints.
	r.GET("/fs/list", s.handleFSList)
	r.GET("/fs/read", s.handleFSRead)

	// Control-dashboard HTTP API (design 005 DB1–DB4). Mutating endpoints 403
	// unless control mode is enabled.
	s.registerControlAPI(r)

	// electronAPI-parity API (roadmap Phase 3): /api/env, /api/workspaces,
	// /api/voice/*. Read-only except transcription; token-gated like all routes.
	s.registerParityAPI(r)

	// The listener is already bound (see the net.Listen above, which also
	// resolved the bind address — loopback by default, control mode forced
	// onto loopback per design 005 DB4).
	s.httpSrv = &http.Server{Handler: r}
	// Kept so a shared door can be opened later on the SAME routes, hub and
	// session — one server, a second way in (see StartShared).
	s.engine = r

	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("web server error", "err", err)
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}
	}()

	return nil
}

// Stop gracefully shuts down the web server.
func (s *Server) Stop() error {
	s.mu.Lock()

	// A dead instance (running already false after an async Serve error) still
	// owns subscriptions and an http.Server — tear everything down regardless,
	// so Stop is the one call that always leaves the server fully released.
	wasRunning := s.running
	if !wasRunning && s.httpSrv == nil {
		s.mu.Unlock()
		return fmt.Errorf("web server is not running")
	}

	s.running = false
	// The shared door goes with the server it is a door onto — leaving a
	// listener up after Stop would serve a session nothing is driving.
	if s.sharedSrv != nil {
		_ = s.sharedSrv.Close()
		s.sharedSrv, s.sharedLn = nil, nil
		s.sharedHost, s.sharedPort = "", 0
	}
	if s.approSub != nil {
		_ = s.approSub.Unsubscribe()
		s.approSub = nil
	}
	if s.browserSub != nil {
		_ = s.browserSub.Unsubscribe()
		s.browserSub = nil
	}
	if s.webPromptSub != nil {
		_ = s.webPromptSub.Unsubscribe()
		s.webPromptSub = nil
	}
	if s.webActivateSub != nil {
		_ = s.webActivateSub.Unsubscribe()
		s.webActivateSub = nil
	}
	if s.webDeactivateSub != nil {
		_ = s.webDeactivateSub.Unsubscribe()
		s.webDeactivateSub = nil
	}
	if s.importCookiesSub != nil {
		_ = s.importCookiesSub.Unsubscribe()
		s.importCookiesSub = nil
	}
	if s.pipelineSub != nil {
		_ = s.pipelineSub.Unsubscribe()
		s.pipelineSub = nil
	}
	if s.emailListSub != nil {
		_ = s.emailListSub.Unsubscribe()
		s.emailListSub = nil
	}
	if s.emailDetailSub != nil {
		_ = s.emailDetailSub.Unsubscribe()
		s.emailDetailSub = nil
	}
	if s.emailChangedSub != nil {
		_ = s.emailChangedSub.Unsubscribe()
		s.emailChangedSub = nil
	}
	if s.waListSub != nil {
		_ = s.waListSub.Unsubscribe()
		s.waListSub = nil
	}
	if s.waDetailSub != nil {
		_ = s.waDetailSub.Unsubscribe()
		s.waDetailSub = nil
	}
	if s.waChangedSub != nil {
		_ = s.waChangedSub.Unsubscribe()
		s.waChangedSub = nil
	}
	if s.pairingSub != nil {
		_ = s.pairingSub.Unsubscribe()
		s.pairingSub = nil
	}
	if s.layoutSub != nil {
		_ = s.layoutSub.Unsubscribe()
		s.layoutSub = nil
	}
	for _, sub := range s.paneOutSubs {
		_ = sub.Unsubscribe()
	}
	s.paneOutSubs = nil
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	srv := s.httpSrv
	s.httpSrv = nil
	s.mu.Unlock()

	// Tear down any server-side embedded browsers (W12) so headless Chromium
	// processes never outlive the web session.
	s.closeAllWebPanes()

	if srv != nil {
		// Shutdown OUTSIDE the lock: in-flight handlers take s.mu (requireControl
		// → ControlEnabled, Host), so holding it here deadlocks them against the
		// grace period below.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
	return nil
}

// IsRunning reports whether the server is currently running.
func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Port returns the configured port number.
func (s *Server) Port() int {
	return s.port
}

// ---------------------------------------------------------------------------
// WebSocket
// ---------------------------------------------------------------------------

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

func (s *Server) handleWebSocket(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	stream := c.Query("stream") == "1"
	client := &wsClient{
		hub:           s.hub,
		conn:          conn,
		send:          make(chan []byte, 256),
		streamContent: stream,
		sizeClientID:  fmt.Sprintf("web:%d", s.nextClientSeq.Add(1)),
		sizedPanes:    make(map[string]bool),
	}
	// Stream clients render from a layout-only snapshot + per-pane content
	// deltas. Seed them with the layout, then the content in byte-bounded
	// batches — one 17 MB snapshot could not be written inside writeWait over a
	// tunnel, which left the browser permanently blank. See seed.go.
	//
	// Deliberately BEFORE hub registration: the hub owns close(client.send), so
	// sending to an unregistered client's buffer cannot race a close, and the
	// seed gets the buffer to itself.
	if stream {
		s.seedStreamClient(client)
	}
	s.hub.register <- client

	go client.writePump()
	go client.readPump(s)
}

// ---------------------------------------------------------------------------
// Snapshot polling
// ---------------------------------------------------------------------------

func (s *Server) pollSnapshots(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// The blind 200ms full-snapshot poll serves only clients that did NOT
			// opt into the content plane. Stream clients instead receive
			// layout-only snapshots on ws.layoutDirty plus per-pane content deltas.
			//
			// Ask whether such a client exists BEFORE building the snapshot. With
			// layoutOnly=false the reply carries every pane's content — measured at
			// 4.4 MB on a 50-pane session — so polling it five times a second and
			// then discarding it in sendWhere cost ~22 MB/s of bus traffic for
			// nothing whenever every connected client was a stream client.
			if !s.hub.hasPlain() {
				continue
			}
			data := s.snapshotMessage(false, false)
			if data == nil {
				continue
			}
			s.hub.sendWhere(data, func(c *wsClient) bool { return !c.streamContent })
		}
	}
}

// snapshotMessage fetches a workspace snapshot and marshals the WS envelope.
// layoutOnly strips per-pane content (output/history/VT) — streamed separately
// to stream clients, which rehydrate from their accumulated per-pane buffers.
// The layout_only flag tells the client whether to (re)seed its content store
// from this snapshot (full) or keep its store and use this for layout only.
func (s *Server) snapshotMessage(layoutOnly, fresh bool) []byte {
	return s.snapshotMessageOpts(layoutOnly, fresh, false)
}

// snapshotMessageRaw returns the decoded workspace snapshot rather than an
// encoded message. The seed path needs the panes themselves so it can cut them
// into byte-bounded batches (see seed.go).
func (s *Server) snapshotMessageRaw(layoutOnly, fresh bool) *domain.WorkspaceSnapshot {
	reply, err := s.pub.Request(msg.T("ws", "snapshot"),
		&msg.MsgGetWorkspaceSnapshot{LayoutOnly: layoutOnly, Fresh: fresh}, 5*time.Second)
	if err != nil {
		return nil
	}
	r, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return nil
	}
	return &r.Snapshot
}

// snapshotMessageOpts is snapshotMessage with the histories switch exposed. The
// seed path uses it; everything else keeps the previous behaviour.
func (s *Server) snapshotMessageOpts(layoutOnly, fresh, noHistories bool) []byte {
	reply, err := s.pub.Request(msg.T("ws", "snapshot"),
		&msg.MsgGetWorkspaceSnapshot{LayoutOnly: layoutOnly, Fresh: fresh, NoHistories: noHistories}, 2*time.Second)
	if err != nil {
		return nil
	}
	r, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return nil
	}
	data, err := json.Marshal(map[string]interface{}{
		"type":        "snapshot",
		"data":        &r.Snapshot,
		"layout_only": layoutOnly,
	})
	if err != nil {
		return nil
	}
	return data
}

// ---------------------------------------------------------------------------
// Approval request forwarding
// ---------------------------------------------------------------------------

func (s *Server) subscribeApprovalRequests() {
	sub, err := s.nc.Subscribe(msg.T("pane", "*", "approval", "request"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		req, ok := decoded.(*msg.MsgApprovalRequest)
		if !ok {
			return
		}

		// Extract pane ID from subject: {session}.pane.<paneID>.approval.request
		parts := strings.Split(natMsg.Subject, ".")
		paneID := ""
		if len(parts) >= 3 {
			paneID = parts[2]
		}

		data, err := json.Marshal(map[string]interface{}{
			"type": "approval_request",
			"data": map[string]interface{}{
				"pane_id": paneID,
				"request": req,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	})
	if err != nil {
		s.logger.Error("failed to subscribe to approval requests", "err", err)
		return
	}
	s.approSub = sub
}

// subscribeBrowserRequests forwards browser-action requests (emitted by a pane's
// browser_action tool) to WebSocket clients. The web pane executes the action on
// its embedded WebContentsView and replies via the "browser_result" command,
// which handleCommand publishes back on the pane's browser.response subject.
func (s *Server) subscribeBrowserRequests() {
	sub, err := s.nc.Subscribe(msg.T("pane", "*", "browser", "request"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		req, ok := decoded.(*msg.MsgBrowserActionRequest)
		if !ok {
			return
		}

		// Extract pane ID from subject: {session}.pane.<paneID>.browser.request
		parts := strings.Split(natMsg.Subject, ".")
		paneID := ""
		if len(parts) >= 3 {
			paneID = parts[2]
		}

		data, err := json.Marshal(map[string]interface{}{
			"type": "browser_action",
			"data": map[string]interface{}{
				"pane_id": paneID,
				"request": req,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	})
	if err != nil {
		s.logger.Error("failed to subscribe to browser requests", "err", err)
		return
	}
	s.browserSub = sub
}

// subscribeWebPrompts forwards web-AI prompt notices (published by
// `##mode web ai <prompt>`) to WebSocket clients as a "web_prompt" message, so
// the desktop app renders the prompt as a human bubble in the pane's Ask Rysh
// panel — identical to a prompt typed into the in-pane chat box.
func (s *Server) subscribeWebPrompts() {
	sub, err := s.nc.Subscribe(msg.T("pane", "*", "web", "prompt"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		note, ok := decoded.(*msg.MsgWebPromptDispatched)
		if !ok {
			return
		}

		// Subject: {session}.pane.<paneID>.web.prompt
		paneID := note.PaneID
		if paneID == "" {
			parts := strings.Split(natMsg.Subject, ".")
			if len(parts) >= 3 {
				paneID = parts[2]
			}
		}

		data, err := json.Marshal(map[string]interface{}{
			"type": "web_prompt",
			"data": map[string]interface{}{
				"pane_id": paneID,
				"prompt":  note.Prompt,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	})
	if err != nil {
		s.logger.Error("failed to subscribe to web prompts", "err", err)
		return
	}
	s.webPromptSub = sub
}

// subscribeWebActivate forwards web-activate notices (published by
// `##mode new web`) to WebSocket clients as a "web_activate" message, so the
// desktop app deterministically switches the pane to web display mode without
// waiting to notice a snapshot's web_activate_seq change.
func (s *Server) subscribeWebActivate() {
	sub, err := s.nc.Subscribe(msg.T("pane", "*", "web", "activate"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		note, ok := decoded.(*msg.MsgWebActivate)
		if !ok {
			return
		}

		// Subject: {session}.pane.<paneID>.web.activate
		paneID := note.PaneID
		if paneID == "" {
			parts := strings.Split(natMsg.Subject, ".")
			if len(parts) >= 3 {
				paneID = parts[2]
			}
		}

		data, err := json.Marshal(map[string]interface{}{
			"type": "web_activate",
			"data": map[string]interface{}{
				"pane_id": paneID,
				"profile": note.Profile,
				"url":     note.URL,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	})
	if err != nil {
		s.logger.Error("failed to subscribe to web activate", "err", err)
		return
	}
	s.webActivateSub = sub
}

// subscribeImportCookies forwards cookie-import notices (published by
// `##web import-google-session`) to WebSocket clients as an "import_cookies"
// message, so the desktop app writes the cookies into the profile's persistent
// session partition. Profile-global, not pane-scoped.
func (s *Server) subscribeImportCookies() {
	sub, err := s.nc.Subscribe(msg.T("web", "import-cookies"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		note, ok := decoded.(*msg.MsgPaneImportCookies)
		if !ok {
			return
		}

		data, err := json.Marshal(map[string]interface{}{
			"type": "import_cookies",
			"data": map[string]interface{}{
				"profile": note.Profile,
				"cookies": note.Cookies,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	})
	if err != nil {
		s.logger.Error("failed to subscribe to import cookies", "err", err)
		return
	}
	s.importCookiesSub = sub
}

// subscribeWebDeactivate forwards web-deactivate notices (published by
// `##mode delete web`) to WebSocket clients as a "web_deactivate" message, so
// the desktop app deterministically drops the pane off web display mode without
// waiting to notice a snapshot's cleared web binding.
func (s *Server) subscribeWebDeactivate() {
	sub, err := s.nc.Subscribe(msg.T("pane", "*", "web", "deactivate"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		note, ok := decoded.(*msg.MsgWebDeactivate)
		if !ok {
			return
		}

		// Subject: {session}.pane.<paneID>.web.deactivate
		paneID := note.PaneID
		if paneID == "" {
			parts := strings.Split(natMsg.Subject, ".")
			if len(parts) >= 3 {
				paneID = parts[2]
			}
		}

		data, err := json.Marshal(map[string]interface{}{
			"type": "web_deactivate",
			"data": map[string]interface{}{
				"pane_id": paneID,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	})
	if err != nil {
		s.logger.Error("failed to subscribe to web deactivate", "err", err)
		return
	}
	s.webDeactivateSub = sub
}

// subscribeEmailEvents forwards an email humanoid's desktop-client events to
// WebSocket clients: inbox listings ("email_list"), single-email details
// ("email_detail"), and inbox-changed notices ("email_inbox_changed"). Each is
// published by the humanoid on humanoid.<name>.email.<kind> (see
// handleEmailListQuery / handleEmailReadQuery / the inbound path). Events carry
// humanoid_name so a client ignores data for a humanoid it isn't showing.
func (s *Server) subscribeEmailEvents() {
	if sub, err := s.nc.Subscribe(msg.T("humanoid", "*", "email", "list"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		r, ok := decoded.(*msg.MsgHumanoidEmailListReply)
		if !ok {
			return
		}
		data, err := json.Marshal(map[string]interface{}{
			"type": "email_list",
			"data": map[string]interface{}{
				"humanoid_name": r.HumanoidName,
				"emails":        r.Emails,
				"err":           r.Err,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	}); err != nil {
		s.logger.Error("failed to subscribe to email list", "err", err)
	} else {
		s.emailListSub = sub
	}

	if sub, err := s.nc.Subscribe(msg.T("humanoid", "*", "email", "detail"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		r, ok := decoded.(*msg.MsgHumanoidEmailReadReply)
		if !ok {
			return
		}
		data, err := json.Marshal(map[string]interface{}{
			"type": "email_detail",
			"data": map[string]interface{}{
				"humanoid_name": r.HumanoidName,
				"email":         r.Email,
				"err":           r.Err,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	}); err != nil {
		s.logger.Error("failed to subscribe to email detail", "err", err)
	} else {
		s.emailDetailSub = sub
	}

	if sub, err := s.nc.Subscribe(msg.T("humanoid", "*", "email", "changed"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		r, ok := decoded.(*msg.MsgHumanoidEmailChanged)
		if !ok {
			return
		}
		data, err := json.Marshal(map[string]interface{}{
			"type": "email_inbox_changed",
			"data": map[string]interface{}{
				"humanoid_name": r.HumanoidName,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	}); err != nil {
		s.logger.Error("failed to subscribe to email changed", "err", err)
	} else {
		s.emailChangedSub = sub
	}
}

// subscribeWhatsAppEvents forwards a WhatsApp humanoid's desktop-client events to
// WebSocket clients: recent-message listings ("whatsapp_list"), single-message
// details ("whatsapp_detail"), and inbox-changed notices ("whatsapp_inbox_changed").
// Mirrors subscribeEmailEvents.
func (s *Server) subscribeWhatsAppEvents() {
	if sub, err := s.nc.Subscribe(msg.T("humanoid", "*", "whatsapp", "list"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		r, ok := decoded.(*msg.MsgHumanoidWhatsAppListReply)
		if !ok {
			return
		}
		data, err := json.Marshal(map[string]interface{}{
			"type": "whatsapp_list",
			"data": map[string]interface{}{
				"humanoid_name": r.HumanoidName,
				"messages":      r.Messages,
				"err":           r.Err,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	}); err != nil {
		s.logger.Error("failed to subscribe to whatsapp list", "err", err)
	} else {
		s.waListSub = sub
	}

	if sub, err := s.nc.Subscribe(msg.T("humanoid", "*", "whatsapp", "detail"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		r, ok := decoded.(*msg.MsgHumanoidWhatsAppReadReply)
		if !ok {
			return
		}
		data, err := json.Marshal(map[string]interface{}{
			"type": "whatsapp_detail",
			"data": map[string]interface{}{
				"humanoid_name": r.HumanoidName,
				"message":       r.Message,
				"err":           r.Err,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	}); err != nil {
		s.logger.Error("failed to subscribe to whatsapp detail", "err", err)
	} else {
		s.waDetailSub = sub
	}

	if sub, err := s.nc.Subscribe(msg.T("humanoid", "*", "whatsapp", "changed"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		r, ok := decoded.(*msg.MsgHumanoidWhatsAppChanged)
		if !ok {
			return
		}
		data, err := json.Marshal(map[string]interface{}{
			"type": "whatsapp_inbox_changed",
			"data": map[string]interface{}{
				"humanoid_name": r.HumanoidName,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	}); err != nil {
		s.logger.Error("failed to subscribe to whatsapp changed", "err", err)
	} else {
		s.waChangedSub = sub
	}
}

// ---------------------------------------------------------------------------
// Command dispatch
// ---------------------------------------------------------------------------

// handleCommand processes a command received from a WebSocket client and
// publishes the corresponding typed message to the workspace NATS inbox.
func (s *Server) handleCommand(action string, data json.RawMessage) {
	wsInbox := msg.T("ws", "inbox")

	switch action {
	case "create_tab":
		_ = s.pub.Send(wsInbox, &msg.MsgCreateTab{})
	case "create_pane":
		_ = s.pub.Send(wsInbox, &msg.MsgCreatePane{})
	case "create_pane_down":
		_ = s.pub.Send(wsInbox, &msg.MsgCreatePaneDown{})
	case "close_pane":
		_ = s.pub.Send(wsInbox, &msg.MsgClosePane{})
	case "focus_next_tab":
		_ = s.pub.Send(wsInbox, &msg.MsgFocusNextTab{})
	case "focus_prev_tab":
		_ = s.pub.Send(wsInbox, &msg.MsgFocusPrevTab{})
	case "focus_tab_index":
		var cmd struct {
			Index int `json:"index"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			_ = s.pub.Send(wsInbox, &msg.MsgFocusTabIndex{Index: cmd.Index})
		}
	case "move_tab":
		var cmd struct {
			Direction string `json:"direction"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			dir := msg.DirRight
			if cmd.Direction == "left" {
				dir = msg.DirLeft
			}
			_ = s.pub.Send(wsInbox, &msg.MsgMoveTab{Direction: dir})
		}
	case "switch_workspace":
		var cmd struct {
			Index     int    `json:"index"`
			Direction string `json:"direction"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			m := &msg.MsgSwitchWorkspace{Index: cmd.Index}
			switch cmd.Direction {
			case "next":
				m.Direction = msg.DirNext
			case "prev":
				m.Direction = msg.DirPrev
			}
			_ = s.pub.Send(wsInbox, m)
		}
	case "focus_next_pane":
		_ = s.pub.Send(wsInbox, &msg.MsgFocusPane{Direction: msg.DirNext})
	case "focus_prev_pane":
		_ = s.pub.Send(wsInbox, &msg.MsgFocusPane{Direction: msg.DirPrev})
	case "focus_pane_left":
		_ = s.pub.Send(wsInbox, &msg.MsgFocusPane{Direction: msg.DirLeft})
	case "focus_pane_right":
		_ = s.pub.Send(wsInbox, &msg.MsgFocusPane{Direction: msg.DirRight})
	case "focus_pane_up":
		_ = s.pub.Send(wsInbox, &msg.MsgFocusPane{Direction: msg.DirUp})
	case "focus_pane_down":
		_ = s.pub.Send(wsInbox, &msg.MsgFocusPane{Direction: msg.DirDown})
	case "focus_pane_by_id":
		var cmd struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			_ = s.pub.Send(wsInbox, &msg.MsgFocusPaneByID{ID: cmd.ID})
		}
	case "submit_input":
		var cmd struct {
			Text string `json:"text"`
			Mode string `json:"mode"`
			// PaneID (optional) pins the input to the pane whose box it was
			// typed in, instead of the daemon's current active pane — see
			// MsgSubmitInput.PaneID.
			PaneID string `json:"pane_id"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			_ = s.pub.Send(wsInbox, &msg.MsgSubmitInput{Text: cmd.Text, Mode: cmd.Mode, PaneID: cmd.PaneID})
		}
	case "resize_pane":
		var cmd struct {
			Delta int `json:"delta"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			_ = s.pub.Send(wsInbox, &msg.MsgResizePane{Delta: cmd.Delta})
		}
	case "resize_pane_width":
		var cmd struct {
			Delta int `json:"delta"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			_ = s.pub.Send(wsInbox, &msg.MsgResizePaneWidth{Delta: cmd.Delta})
		}
	case "resize_pane_height":
		var cmd struct {
			Delta int `json:"delta"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			_ = s.pub.Send(wsInbox, &msg.MsgResizePaneHeight{Delta: cmd.Delta})
		}
	case "rename_pane":
		var cmd struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			_ = s.pub.Send(wsInbox, &msg.MsgRenamePane{Title: cmd.Title})
		}
	case "rename_tab":
		var cmd struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			_ = s.pub.Send(wsInbox, &msg.MsgRenameTab{Title: cmd.Title})
		}
	case "rename_lane":
		var cmd struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			_ = s.pub.Send(wsInbox, &msg.MsgRenameLane{Name: cmd.Name})
		}
	case "create_stacked_pane":
		_ = s.pub.Send(wsInbox, &msg.MsgCreateStackedPane{})
	case "stacked_pane_next":
		_ = s.pub.Send(wsInbox, &msg.MsgStackedPaneRotate{Direction: msg.DirNext})
	case "stacked_pane_prev":
		_ = s.pub.Send(wsInbox, &msg.MsgStackedPaneRotate{Direction: msg.DirPrev})
	case "stacked_pane_select":
		var cmd struct {
			Index int `json:"index"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			_ = s.pub.Send(wsInbox, &msg.MsgStackedPaneSelect{Index: cmd.Index})
		}
	case "stacked_pane_move":
		var cmd struct {
			Direction string `json:"direction"`
		}
		if json.Unmarshal(data, &cmd) == nil {
			dir := msg.DirUp
			if cmd.Direction == "down" {
				dir = msg.DirDown
			}
			_ = s.pub.Send(wsInbox, &msg.MsgStackedPaneMove{Direction: dir})
		}
	case "toggle_pipeline_mode":
		_ = s.pub.Send(wsInbox, &msg.MsgTogglePipelineMode{})
	case "equalize_panes":
		_ = s.pub.Send(wsInbox, &msg.MsgEqualizePanes{})
	case "equalize_horizontal":
		_ = s.pub.Send(wsInbox, &msg.MsgEqualizeHorizontal{})
	case "equalize_vertical":
		_ = s.pub.Send(wsInbox, &msg.MsgEqualizeVertical{})
	case "equalize_all":
		_ = s.pub.Send(wsInbox, &msg.MsgEqualizeAll{})
	case "swap_pane":
		_ = s.pub.Send(wsInbox, &msg.MsgSwapPane{})

	// Approval response: user accepted/rejected a tool action.
	case "approval_response":
		var cmd struct {
			PaneID    string `json:"pane_id"`
			RequestID string `json:"request_id"`
			Decision  string `json:"decision"`
			Reason    string `json:"reason"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.PaneID != "" {
			subject := msg.T("pane", cmd.PaneID, "approval", "response")
			resp := &msg.MsgApprovalResponse{
				RequestID: cmd.RequestID,
				Decision:  msg.ApprovalDecision(cmd.Decision),
				Reason:    cmd.Reason,
			}
			_ = s.pub.Send(subject, resp)

			// Also echo the decision to the pane's output.
			label := "approved"
			switch msg.ApprovalDecision(cmd.Decision) {
			case msg.DecisionNo, msg.DecisionNoWithExplanation:
				label = "rejected"
			case msg.DecisionYesAlways:
				label = "approved (always)"
			}
			_ = s.pub.SendPaneAIOutput(cmd.PaneID, fmt.Sprintf("[%s]\n", label))
		}

	// -----------------------------------------------------------------------
	// Browser-action result (web pane → AI's browser_action tool)
	// -----------------------------------------------------------------------
	case "browser_result":
		var cmd struct {
			PaneID     string          `json:"pane_id"`
			RequestID  string          `json:"request_id"`
			Success    bool            `json:"success"`
			Result     json.RawMessage `json:"result"`
			Error      string          `json:"error"`
			Screenshot string          `json:"screenshot"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.PaneID != "" {
			subject := msg.T("pane", cmd.PaneID, "browser", "response")
			_ = s.pub.Send(subject, &msg.MsgBrowserActionResponse{
				RequestID:  cmd.RequestID,
				Success:    cmd.Success,
				Result:     cmd.Result,
				Error:      cmd.Error,
				Screenshot: cmd.Screenshot,
			})
		}

	// -----------------------------------------------------------------------
	// Raw key input (data-plane bypass for interactive terminal)
	// -----------------------------------------------------------------------
	case "raw_key_input":
		var cmd struct {
			PaneID string `json:"pane_id"`
			Data   string `json:"data"` // base64-encoded
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.PaneID != "" && cmd.Data != "" {
			rawBytes, err := base64.StdEncoding.DecodeString(cmd.Data)
			if err == nil {
				subject := msg.T("pane", cmd.PaneID, "rawinput")
				_ = s.pub.Send(subject, &msg.MsgRawKeyInput{PaneID: cmd.PaneID, Data: rawBytes})
			}
		}

	// "pane_resize" is deliberately NOT here: it needs the originating client,
	// because a resize is a per-viewport size claim rather than a broadcast
	// command. It is handled in handleClientCommand → handlePaneResize.

	case "pane_clear_output":
		// Readline Ctrl+L from the desktop app: clear the pane display buffers
		// without recording a history entry (same as the TUI gesture).
		var cmd struct {
			PaneID string `json:"pane_id"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.PaneID != "" {
			_ = s.pub.Send(msg.T("pane", cmd.PaneID, "inbox"), &msg.MsgPaneClearOutput{PaneID: cmd.PaneID})
		}

	case "pane_native_mode":
		// ##native pass-through toggle from the desktop app; Action is
		// "on" | "off" | "toggle" (PaneActor validates/defaults).
		var cmd struct {
			PaneID string `json:"pane_id"`
			Action string `json:"action"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.PaneID != "" {
			_ = s.pub.Send(msg.T("pane", cmd.PaneID, "inbox"), &msg.MsgPaneNativeMode{PaneID: cmd.PaneID, Action: cmd.Action})
		}

	case "agentic_cancel":
		// Ctrl+C from the desktop app in AI mode: PAUSE the in-flight agentic
		// run. The orchestrator cancels its context (stopping the current
		// tool/LLM call), but the conversation and session memory are
		// preserved as a checkpoint — the next AI prompt (e.g. "continue")
		// resumes exactly where it stopped. Same semantics as the TUI's
		// double-Ctrl+C. Addressed to the pane's LLM-execution child inbox.
		var cmd struct {
			PaneID string `json:"pane_id"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.PaneID != "" {
			_ = s.pub.Send(msg.T("pane", cmd.PaneID, "llm_prompt_execution", "inbox"), &msg.MsgAgenticCancel{})
		}

	// -----------------------------------------------------------------------
	// Agent commands
	// -----------------------------------------------------------------------
	case "agent_list":
		agentInbox := msg.T("agent", "registry", "inbox")
		reply, err := s.pub.Request(agentInbox, &msg.MsgAgentList{}, 2*time.Second)
		if err == nil {
			if r, ok := reply.(*msg.MsgAgentListReply); ok {
				resp, _ := json.Marshal(map[string]interface{}{
					"type": "agent_list",
					"data": r.Agents,
				})
				s.hub.sendAll(resp)
			}
		}

	case "agent_create":
		var cmd struct {
			Name         string `json:"name"`
			SystemPrompt string `json:"system_prompt"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.Name != "" {
			agentInbox := msg.T("agent", "registry", "inbox")
			_ = s.pub.Send(agentInbox, &msg.MsgAgentCreate{Name: cmd.Name, SystemPrompt: cmd.SystemPrompt})
		}

	case "agent_delete":
		var cmd struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.Name != "" {
			agentInbox := msg.T("agent", "registry", "inbox")
			_ = s.pub.Send(agentInbox, &msg.MsgAgentDelete{Name: cmd.Name})
		}

	case "agent_activate":
		var cmd struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.Name != "" {
			agentInbox := msg.T("agent", "registry", "inbox")
			_ = s.pub.Send(agentInbox, &msg.MsgAgentActivate{Name: cmd.Name})
		}

	case "agent_deactivate":
		var cmd struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.Name != "" {
			agentInbox := msg.T("agent", "registry", "inbox")
			_ = s.pub.Send(agentInbox, &msg.MsgAgentDeactivate{Name: cmd.Name})
		}

	case "agent_prompt":
		var cmd struct {
			AgentName    string `json:"agent_name"`
			Prompt       string `json:"prompt"`
			SourcePaneID string `json:"source_pane_id"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.AgentName != "" {
			agentInbox := msg.T("agent", "registry", "inbox")
			_ = s.pub.Send(agentInbox, &msg.MsgAgentPrompt{
				AgentName:    cmd.AgentName,
				Prompt:       cmd.Prompt,
				SourcePaneID: cmd.SourcePaneID,
			})
		}

	case "agent_register_output":
		var cmd struct {
			AgentName string `json:"agent_name"`
			PaneID    string `json:"pane_id"`
			PaneName  string `json:"pane_name"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.AgentName != "" {
			agentInbox := msg.T("agent", "registry", "inbox")
			_ = s.pub.Send(agentInbox, &msg.MsgAgentRegisterPane{
				AgentName: cmd.AgentName,
				PaneID:    cmd.PaneID,
				PaneName:  cmd.PaneName,
			})
		}

	case "agent_unregister_output":
		var cmd struct {
			AgentName string `json:"agent_name"`
			PaneID    string `json:"pane_id"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.AgentName != "" {
			agentInbox := msg.T("agent", "registry", "inbox")
			_ = s.pub.Send(agentInbox, &msg.MsgAgentUnregisterPane{
				AgentName: cmd.AgentName,
				PaneID:    cmd.PaneID,
			})
		}

	// -----------------------------------------------------------------------
	// Humanoid commands
	// -----------------------------------------------------------------------
	case "humanoid_list":
		humanoidInbox := msg.T("humanoid", "registry", "inbox")
		reply, err := s.pub.Request(humanoidInbox, &msg.MsgHumanoidList{}, 2*time.Second)
		if err == nil {
			if r, ok := reply.(*msg.MsgHumanoidListReply); ok {
				resp, _ := json.Marshal(map[string]interface{}{
					"type": "humanoid_list",
					"data": r.Humanoids,
				})
				s.hub.sendAll(resp)
			}
		}

	case "humanoid_create":
		var cmd struct {
			Name         string `json:"name"`
			SystemPrompt string `json:"system_prompt"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.Name != "" {
			humanoidInbox := msg.T("humanoid", "registry", "inbox")
			_ = s.pub.Send(humanoidInbox, &msg.MsgHumanoidCreate{
				Name:         cmd.Name,
				SystemPrompt: cmd.SystemPrompt,
				Contacts:     nil,
			})
		}

	case "humanoid_delete":
		var cmd struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.Name != "" {
			humanoidInbox := msg.T("humanoid", "registry", "inbox")
			_ = s.pub.Send(humanoidInbox, &msg.MsgHumanoidDelete{Name: cmd.Name})
		}

	case "humanoid_activate":
		var cmd struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.Name != "" {
			humanoidInbox := msg.T("humanoid", "registry", "inbox")
			_ = s.pub.Send(humanoidInbox, &msg.MsgHumanoidActivate{Name: cmd.Name})
		}

	case "humanoid_deactivate":
		var cmd struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.Name != "" {
			humanoidInbox := msg.T("humanoid", "registry", "inbox")
			_ = s.pub.Send(humanoidInbox, &msg.MsgHumanoidDeactivate{Name: cmd.Name})
		}

	case "humanoid_prompt":
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			Prompt       string `json:"prompt"`
			SourcePaneID string `json:"source_pane_id"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" {
			humanoidInbox := msg.T("humanoid", "registry", "inbox")
			_ = s.pub.Send(humanoidInbox, &msg.MsgHumanoidPrompt{
				HumanoidName: cmd.HumanoidName,
				Prompt:       cmd.Prompt,
				SourcePaneID: cmd.SourcePaneID,
			})
		}

	case "humanoid_channel_start":
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			ChannelType  string `json:"channel_type"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" {
			subject := msg.T("humanoid", cmd.HumanoidName, "inbox")
			_ = s.pub.Send(subject, &msg.MsgHumanoidChannelStart{ChannelType: cmd.ChannelType})
		}

	case "humanoid_channel_stop":
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			ChannelType  string `json:"channel_type"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" {
			subject := msg.T("humanoid", cmd.HumanoidName, "inbox")
			_ = s.pub.Send(subject, &msg.MsgHumanoidChannelStop{ChannelType: cmd.ChannelType})
		}

	// -----------------------------------------------------------------------
	// Email client commands (desktop Gmail-style view). The humanoid serves
	// these from its EmailAdapter and replies asynchronously as the
	// "email_list" / "email_detail" events (see subscribeEmailEvents).
	// -----------------------------------------------------------------------
	case "email_list", "email_refresh":
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			Count        int    `json:"count"`
			Search       string `json:"search"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" {
			subject := msg.T("humanoid", cmd.HumanoidName, "inbox")
			_ = s.pub.Send(subject, &msg.MsgHumanoidEmailList{Count: cmd.Count, Search: cmd.Search})
		}

	case "email_read":
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			UID          int    `json:"uid"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" {
			subject := msg.T("humanoid", cmd.HumanoidName, "inbox")
			_ = s.pub.Send(subject, &msg.MsgHumanoidEmailRead{UID: cmd.UID})
		}

	case "email_focus":
		// The UI opened an email (listing=false) or returned to the inbox list
		// (listing=true). Fire-and-forget; the humanoid uses it to enrich prompts
		// so "reply to this" acts on the email the user is viewing.
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			Listing      bool   `json:"listing"`
			UID          int    `json:"uid"`
			MessageID    string `json:"message_id"`
			ThreadID     string `json:"thread_id"`
			From         string `json:"from"`
			Subject      string `json:"subject"`
			Body         string `json:"body"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" {
			subject := msg.T("humanoid", cmd.HumanoidName, "inbox")
			_ = s.pub.Send(subject, &msg.MsgHumanoidSetFocus{
				Listing:   cmd.Listing,
				UID:       cmd.UID,
				MessageID: cmd.MessageID,
				ThreadID:  cmd.ThreadID,
				From:      cmd.From,
				Subject:   cmd.Subject,
				Body:      cmd.Body,
			})
		}

	// -----------------------------------------------------------------------
	// WhatsApp client commands (desktop view). The humanoid serves these from
	// its in-memory message store and replies as "whatsapp_list" /
	// "whatsapp_detail" events (see subscribeWhatsAppEvents).
	// -----------------------------------------------------------------------
	case "whatsapp_list", "whatsapp_refresh":
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			Count        int    `json:"count"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" {
			subject := msg.T("humanoid", cmd.HumanoidName, "inbox")
			_ = s.pub.Send(subject, &msg.MsgHumanoidWhatsAppList{Count: cmd.Count})
		}

	case "whatsapp_read":
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			ID           string `json:"id"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" {
			subject := msg.T("humanoid", cmd.HumanoidName, "inbox")
			_ = s.pub.Send(subject, &msg.MsgHumanoidWhatsAppRead{ID: cmd.ID})
		}

	case "whatsapp_focus":
		// The UI opened a message (listing=false) or returned to the list
		// (listing=true). Reuses MsgHumanoidSetFocus: From=sender, Body=text.
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			Listing      bool   `json:"listing"`
			ID           string `json:"id"`
			From         string `json:"from"`
			Body         string `json:"body"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" {
			subject := msg.T("humanoid", cmd.HumanoidName, "inbox")
			_ = s.pub.Send(subject, &msg.MsgHumanoidSetFocus{
				Listing:   cmd.Listing,
				MessageID: cmd.ID,
				From:      cmd.From,
				Body:      cmd.Body,
			})
		}

	// -----------------------------------------------------------------------
	// Share commands
	// -----------------------------------------------------------------------
	case "share_list":
		shareInbox := msg.T("share", "registry", "inbox")
		reply, err := s.pub.Request(shareInbox, &msg.MsgShareList{}, 2*time.Second)
		if err == nil {
			if r, ok := reply.(*msg.MsgShareListReply); ok {
				resp, _ := json.Marshal(map[string]interface{}{
					"type": "share_list",
					"data": r.Shares,
				})
				s.hub.sendAll(resp)
			}
		}

	case "share_entity":
		var cmd struct {
			EntityType string `json:"entity_type"`
			EntityID   string `json:"entity_id"`
			Mode       string `json:"mode"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.EntityID != "" {
			shareInbox := msg.T("share", "registry", "inbox")
			_ = s.pub.Send(shareInbox, &msg.MsgShareEntity{
				EntityType: cmd.EntityType,
				EntityID:   cmd.EntityID,
				Mode:       cmd.Mode,
			})
		}

	case "unshare_entity":
		var cmd struct {
			EntityID string `json:"entity_id"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.EntityID != "" {
			shareInbox := msg.T("share", "registry", "inbox")
			_ = s.pub.Send(shareInbox, &msg.MsgUnshareEntity{EntityID: cmd.EntityID})
		}

	// -----------------------------------------------------------------------
	// Remote forward command (raw keystrokes for interactive remote shares)
	// -----------------------------------------------------------------------
	case "remote_forward_command":
		var cmd struct {
			CommandType string `json:"command_type"`
			Payload     string `json:"payload"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.CommandType != "" {
			wsInbox := msg.T("ws", "inbox")
			_ = s.pub.Send(wsInbox, &msg.MsgRemoteForwardCommand{
				CommandType: cmd.CommandType,
				Payload:     cmd.Payload,
			})
		}

	default:
		// Control-dashboard actions (design 005): pairing, governance,
		// reply-mode, control status. Mutating actions are gated on control
		// mode inside handleControlCommand.
		s.handleControlCommand(action, data)
	}
}

// ---------------------------------------------------------------------------
// Pipeline output forwarding
// ---------------------------------------------------------------------------

func (s *Server) subscribePipelineOutput() {
	sub, err := s.nc.Subscribe(msg.T("tab", "*", "pipelineOutput"), func(natMsg *nats.Msg) {
		parts := strings.Split(natMsg.Subject, ".")
		if len(parts) < 3 {
			return
		}
		tabID := parts[2]

		var env msg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		appendMsg, ok := decoded.(*msg.MsgPipelineOutputAppend)
		if !ok {
			return
		}

		data, err := json.Marshal(map[string]interface{}{
			"type": "pipeline_output",
			"data": map[string]interface{}{
				"tab_id": tabID,
				"text":   appendMsg.Text,
			},
		})
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	})
	if err != nil {
		s.logger.Error("failed to subscribe to pipeline output", "err", err)
		return
	}
	s.pipelineSub = sub
}

// ---------------------------------------------------------------------------
// Content plane: layout-only snapshots + per-pane content deltas (?stream=1)
// ---------------------------------------------------------------------------

// subscribeLayoutDirty pushes an event-driven, coalesced layout-only snapshot to
// stream clients whenever the workspace structure changes — replacing the blind
// 200ms poll for those clients.
func (s *Server) subscribeLayoutDirty(ctx context.Context) {
	dirty := make(chan struct{}, 1)
	sub, err := s.nc.Subscribe(msg.T("ws", "layoutDirty"), func(_ *nats.Msg) {
		select {
		case dirty <- struct{}{}:
		default:
		}
	})
	if err != nil {
		s.logger.Error("failed to subscribe to layoutDirty", "err", err)
		return
	}
	s.layoutSub = sub
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-dirty:
				time.Sleep(16 * time.Millisecond) // coalesce a burst into one refresh
				select {
				case <-dirty:
				default:
				}
				// Fresh: a layoutDirty signal means state just changed (e.g. a pane's
				// enabled modes / web binding), so bypass the snapshot cache — stream
				// clients have no fallback poll and would otherwise stick on a stale
				// memoized snapshot built just before the change.
				//
				// NoHistories: a layout-only snapshot still carries every pane's
				// command history, which measured 5.8 MB on a 143-pane session —
				// pushed on EVERY layout change, and too large to write inside
				// writeWait over a tunnel, so it could kill the connection the seed
				// had just successfully established. Clients get history from the
				// seed instead (store.paneHistory), which is why this can drop it.
				if data := s.snapshotMessageOpts(true, true, true); data != nil {
					s.hub.sendWhere(data, func(c *wsClient) bool { return c.streamContent })
				}
			}
		}
	}()
}

// subscribePaneOutput forwards per-pane, per-mode output deltas (ConversationMessage)
// to stream clients, which accumulate them per pane and rehydrate the layout
// snapshot before rendering — the browser analogue of the TUI content stream.
func (s *Server) subscribePaneOutput() {
	// Subscribe with a wildcard on the mode segment so this covers BOTH the fixed
	// modes (shell, ai, chat, rysh, email, slack, chatbot) AND dynamic
	// per-humanoid modes (e.g. "slack-bot"), whose output topic is
	// pane.{id}.output.{humanoid}. Without this, a humanoid mode would only ever
	// update from full snapshots, so the desktop app's slack-bot view stayed
	// stale until an unrelated refresh.
	//
	// The single-token '*' does NOT match the merged pane.{id}.output topic (it
	// has no mode segment), so there is no double-forwarding. The private buffer
	// never publishes to NATS, so no sensitive content leaks through the wildcard.
	sub, err := s.nc.Subscribe(msg.T("pane", "*", "output", "*"), func(natMsg *nats.Msg) {
		paneID := paneIDFromSubject(natMsg.Subject)
		if paneID == "" {
			return
		}
		mode := modeFromSubject(natMsg.Subject)
		if mode == "" {
			return
		}
		text, turnID, ok := s.decodeOutputText(natMsg.Data)
		if !ok || text == "" {
			return
		}
		// turn_id identifies the orchestrator run a chat chunk belongs to (empty
		// for non-conversation messages). The app uses it to accumulate Ask Rysh
		// replies per turn instead of slicing one capped stream by char offsets.
		data, err := json.Marshal(map[string]interface{}{
			"type": "pane_output",
			"data": map[string]interface{}{"pane_id": paneID, "mode": mode, "text": text, "turn_id": turnID},
		})
		if err != nil {
			return
		}
		s.hub.sendWhere(data, func(c *wsClient) bool { return c.streamContent })
	})
	if err != nil {
		s.logger.Error("failed to subscribe to pane output", "err", err)
		return
	}
	s.paneOutSubs = append(s.paneOutSubs, sub)
}

// decodeOutputText extracts the text payload from a pane output NATS message,
// plus the turn ID (orchestrator run ID) for conversation messages — "" for
// legacy/plain appends that carry no turn identity.
func (s *Server) decodeOutputText(data []byte) (string, string, bool) {
	var env msg.NATSEnvelope
	if json.Unmarshal(data, &env) != nil {
		return "", "", false
	}
	decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		return "", "", false
	}
	switch v := decoded.(type) {
	case *msg.MsgConversationAppend:
		if v.Message != nil {
			return v.Message.Content, v.Message.TurnID, true
		}
	case *msg.MsgPaneOutputAppend:
		return v.Text, "", true
	case *msg.MsgPaneAIOutputAppend:
		return v.Text, "", true
	case *msg.MsgPaneChatOutputAppend:
		return v.Text, "", true
	case *msg.MsgPaneRyshOutputAppend:
		return v.Text, "", true
	case *msg.MsgPaneExternalOutputAppend:
		return v.Text, "", true
	}
	return "", "", false
}

// paneIDFromSubject extracts {paneID} from {prefix}.pane.{paneID}.output.{mode},
// locating the ".pane." marker so a dotted session prefix is handled correctly.
func paneIDFromSubject(subject string) string {
	const marker = ".pane."
	i := strings.Index(subject, marker)
	if i < 0 {
		return ""
	}
	rest := subject[i+len(marker):]
	if j := strings.IndexByte(rest, '.'); j >= 0 {
		return rest[:j]
	}
	return rest
}

// modeFromSubject extracts {mode} from {prefix}.pane.{paneID}.output.{mode}.
// Returns "" when the subject has no .output.{mode} suffix (e.g. the merged
// pane.{id}.output topic, which carries no mode segment). Pane IDs and mode
// names contain no dots, so the mode is the final segment after ".output.".
func modeFromSubject(subject string) string {
	const marker = ".output."
	i := strings.LastIndex(subject, marker)
	if i < 0 {
		return ""
	}
	return subject[i+len(marker):]
}

// streamPaneVT keeps interactive panes' VT screens fresh for stream clients.
// Layout-only snapshots omit vt_screen, and the per-pane output stream carries
// text (not VT frames), so — while any stream client is connected — this finds
// interactive panes from a cheap layout snapshot and fast-pulls each one's VT
// frame, forwarding a pane_vt envelope. paneVTFrame picks the cheapest request
// that can carry the fields, the same split the TUI makes.
func (s *Server) streamPaneVT(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	// Panes reported interactive on the previous tick. When a pane's interactive
	// program exits it simply drops out of interactivePanes and the poll stops
	// pushing frames for it — but stream clients latch interactivity from the
	// pane_vt delta (raw-mode transitions don't emit a layout snapshot), so
	// without an explicit clear they'd stay stuck on the program's last VT frame.
	// Track the set so a pane that just left it gets one raw_mode=false frame,
	// flipping the client back to normal output. A fresh stream client re-seeds
	// interactivity from its initial full snapshot, so dropping this state when no
	// stream clients are connected is safe.
	prevInteractive := map[string]bool{}
	// Hash of the last pane_vt frame actually BROADCAST for each pane. The poll
	// runs at a fixed 10Hz per interactive pane whether or not anything moved, so
	// an idle vim or claude pane re-sent a byte-identical frame ten times a
	// second: measured at 240 msgs/s over 24 idle panes, ~650 KB/s to a single
	// client, all of it redundant. The TUI has always deduped its side of this
	// (vtFrameHash, tui/content_stream.go); the web server never did.
	//
	// Skipping an unchanged frame cannot starve a client: a stream client is
	// seeded with a FULL snapshot on connect (handleWebSocket), which carries
	// vt_screen for every pane, and every later CHANGE is still broadcast.
	// Cleared with prevInteractive when the last stream client leaves, so the
	// next one starts from a clean slate rather than inheriting stale hashes.
	lastFrame := map[string]uint64{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.hub.hasStream() {
				if len(prevInteractive) > 0 {
					prevInteractive = map[string]bool{}
				}
				if len(lastFrame) > 0 {
					lastFrame = map[string]uint64{}
				}
				continue
			}
			// NoHistories: this loop reads pane ids and the RawMode /
			// RemoteInteractive flags and nothing else, but a layout-only snapshot
			// still carries each pane's command history — measured at 28.9 KB of a
			// 29.9 KB pane reply, the same seeded file duplicated across every pane
			// (F-7c). At 10Hz over 50 panes that was ~7.5 MB/s of bus traffic for
			// bytes discarded on arrival.
			reply, err := s.pub.Request(msg.T("ws", "snapshot"),
				&msg.MsgGetWorkspaceSnapshot{LayoutOnly: true, NoHistories: true}, time.Second)
			if err != nil {
				continue
			}
			r, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
			if !ok {
				continue
			}
			current := map[string]bool{}
			for _, pane := range interactivePanes(&r.Snapshot) {
				// Mark interactive before any per-pane fetch error so a transient
				// snapshot failure doesn't spuriously clear the pane next tick.
				current[pane.id] = true
				data, ok := s.paneVTFrame(pane)
				if !ok {
					continue
				}
				// Identical bytes render identically, so there is nothing for a
				// client to do with a repeat. Hash the encoded frame rather than the
				// fields: it is exactly what the client would receive, so it cannot
				// drift from the comparison the skip is claiming to make.
				h := frameHash(data)
				if prev, seen := lastFrame[pane.id]; seen && prev == h {
					continue
				}
				lastFrame[pane.id] = h
				s.hub.sendWhere(data, func(c *wsClient) bool { return c.streamContent })
			}
			// Panes that were interactive last tick but aren't now: push one
			// clearing frame so stream clients leave the VT view.
			for paneID := range prevInteractive {
				if current[paneID] {
					continue
				}
				// Drop the remembered frame too, so a pane that goes interactive
				// again re-sends its first frame instead of being deduped against
				// what it looked like before the program exited.
				delete(lastFrame, paneID)
				data, err := json.Marshal(map[string]interface{}{
					"type": "pane_vt",
					"data": map[string]interface{}{
						"pane_id":            paneID,
						"raw_mode":           false,
						"remote_interactive": false,
					},
				})
				if err != nil {
					continue
				}
				s.hub.sendWhere(data, func(c *wsClient) bool { return c.streamContent })
			}
			prevInteractive = current
		}
	}
}

// frameHash is an FNV-1a hash of an encoded pane_vt frame. Mirrors the TUI's
// vtFrameHash (tui/content_stream.go) in purpose: two frames with the same hash
// render identically, so the second one need not be sent.
func frameHash(data []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(data)
	return h.Sum64()
}

// interactivePane is one pane streamPaneVT must refresh, together with how to
// refresh it cheaply. localRaw marks the panes eligible for the VT-only pull.
type interactivePane struct {
	id       string
	localRaw bool
}

// isMirrorPaneID reports whether an id names a synthetic mirror pane. Mirror
// panes have no PaneActor, so they can never answer MsgGetPaneVT — same rule the
// TUI applies in isLocalRawPane (tui/content_stream.go:565).
func isMirrorPaneID(id string) bool { return strings.HasPrefix(id, "mirror") }

// isLocalRawPane reports whether a pane carries ONLY a local VT frame, so the
// lightweight MsgGetPaneVT reply is sufficient. Remote-interactive and mirror
// panes carry extra state (RemoteVTScreen and friends) that MsgPaneVTReply does
// not model at all, so they must keep the full snapshot.
func isLocalRawPane(p *domain.PaneSnapshot) bool {
	return p.RawMode && !p.RemoteInteractive && !isMirrorPaneID(p.ID)
}

// interactivePanes returns the panes showing a VT screen (local raw or
// remote-interactive) in the snapshot, each classified for the cheap path.
func interactivePanes(snap *domain.WorkspaceSnapshot) []interactivePane {
	var out []interactivePane
	for p := range domain.PanesInWorkspace(snap) {
		if p.RawMode || p.RemoteInteractive {
			out = append(out, interactivePane{id: p.ID, localRaw: isLocalRawPane(p)})
		}
	}
	return out
}

// paneVTFrame builds one pane_vt envelope, taking the cheapest route that can
// carry the fields the client needs.
//
// A LOCAL raw pane pulls MsgGetPaneVT: screen + cursor only, measured at 1.4 KB
// against 29.3 KB for the full snapshot on live panes — 21x smaller, and the
// full reply's remaining bytes are almost entirely shell_history (97%, see F-7c)
// that this stream never reads. A remote-interactive or mirror pane still pulls
// the full snapshot, because MsgPaneVTReply has no Remote* fields to carry.
//
// This is the split tui/content_stream.go:579 has always made; the web server
// never had it, which is why a daemon with no TUI attached issued 100%
// MsgGetPaneSnapshot and zero MsgGetPaneVT (F-7b).
func (s *Server) paneVTFrame(pane interactivePane) ([]byte, bool) {
	subject := msg.T("pane", pane.id, "snapshot")

	if pane.localRaw {
		reply, err := s.pub.Request(subject, &msg.MsgGetPaneVT{}, time.Second)
		if err != nil {
			return nil, false
		}
		r, ok := reply.(*msg.MsgPaneVTReply)
		if !ok {
			return nil, false
		}
		// r.Interactive is the same computeInteractive() gate that sets RawMode on
		// the full snapshot (actors/pane_snapshot.go:150 and :268), so it maps
		// straight onto raw_mode. The remote_* fields stay at their zero values —
		// which is exactly what the full snapshot carried for a !RemoteInteractive
		// pane, so the wire shape the client sees is unchanged.
		data, err := json.Marshal(map[string]interface{}{
			"type": "pane_vt",
			"data": map[string]interface{}{
				"pane_id":              pane.id,
				"raw_mode":             r.Interactive,
				"vt_screen":            r.Screen,
				"vt_cursor_row":        r.CursorRow,
				"vt_cursor_col":        r.CursorCol,
				"remote_interactive":   false,
				"remote_vt_screen":     nil,
				"remote_vt_cursor_row": 0,
				"remote_vt_cursor_col": 0,
			},
		})
		if err != nil {
			return nil, false
		}
		return data, true
	}

	preply, err := s.pub.Request(subject, &msg.MsgGetPaneSnapshot{}, time.Second)
	if err != nil {
		return nil, false
	}
	pr, ok := preply.(*msg.MsgPaneSnapshotReply)
	if !ok {
		return nil, false
	}
	p := pr.Snapshot
	data, err := json.Marshal(map[string]interface{}{
		"type": "pane_vt",
		"data": map[string]interface{}{
			"pane_id":              pane.id,
			"raw_mode":             p.RawMode,
			"vt_screen":            p.VTScreen,
			"vt_cursor_row":        p.VTCursorRow,
			"vt_cursor_col":        p.VTCursorCol,
			"remote_interactive":   p.RemoteInteractive,
			"remote_vt_screen":     p.RemoteVTScreen,
			"remote_vt_cursor_row": p.RemoteVTCursorRow,
			"remote_vt_cursor_col": p.RemoteVTCursorCol,
		},
	})
	if err != nil {
		return nil, false
	}
	return data, true
}

// ---------------------------------------------------------------------------
// prefixFS wraps an fs.FS to prepend a directory prefix to all Open calls.
// This allows serving a subdirectory (e.g., "assets") at a Gin route without
// the prefix appearing in the URL path.
// ---------------------------------------------------------------------------

type prefixFS struct {
	inner  fs.FS
	prefix string
}

func newPrefixFS(inner fs.FS, prefix string) *prefixFS {
	return &prefixFS{inner: inner, prefix: prefix}
}

func (p *prefixFS) Open(name string) (fs.File, error) {
	return p.inner.Open(path.Join(p.prefix, name))
}
