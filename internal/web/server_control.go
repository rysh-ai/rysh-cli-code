// Control-dashboard surface (WS5, design 005 — tasks DB1–DB4).
//
// This file adds the browser control plane on top of the read-only viewer in
// server.go:
//
//   - DB1  channel operations  → MsgHumanoidChannelStart/Stop on
//     humanoid.{name}.inbox (same messages ##humanoid channel start/stop send)
//   - DB2  pairing operations  → MsgChannelPairApprove / MsgChannelAllow /
//     MsgChannelPairList on humanoid.{name}.pairing (same subject the
//     terminal ##humanoid pair commands ride, WS3 / design 003 §4.4), plus a
//     live relay of MsgChannelPairRequest/QR/Status/ListReply into the WS hub
//   - DB3  humanoid operations → MsgHumanoidActivate/Deactivate on the
//     registry inbox and MsgHumanoidSetGovernance / MsgHumanoidSetReplyMode
//     on humanoid.{name}.inbox
//   - DB4  control gating      → every MUTATING endpoint/action requires
//     control mode; read/list endpoints stay available. Control mode is
//     enabled by the RYSH_WEB_CONTROL environment variable (1/true/yes/on)
//     read in NewServer — chosen as the least-invasive wiring so the two
//     existing WorkspaceActor NewServer call sites need no changes — or
//     programmatically via SetControl (tests, future `rysh web --control`).
//     When control is enabled the HTTP listener is forced onto 127.0.0.1.
//
// Terminal and web drive the SAME typed messages on the SAME subjects, so the
// two fronts cannot diverge (design 003 §4.4 / 005 G5).
package web

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// envWebControl is the environment variable that enables control mode.
const envWebControl = "RYSH_WEB_CONTROL"

// controlFromEnv reports whether RYSH_WEB_CONTROL requests control mode.
func controlFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envWebControl))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// SetControl enables or disables control mode. Must be called before Start:
// the listen address is computed at Start time and mutating endpoints consult
// the flag per-request under the server mutex.
func (s *Server) SetControl(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.control = enabled
}

// ControlEnabled reports whether control mode is enabled.
func (s *Server) ControlEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.control
}

// listenAddr returns the HTTP listen address.
//
// With no host configured the server binds DefaultHost (127.0.0.1), so the web
// UI is private to the machine running the daemon until the operator asks for
// something wider. SetHost pins any other bind — 0.0.0.0 for every interface, a
// LAN IP, or a loopback alias like 127.0.100.1 fronted by a reverse proxy.
//
// In control mode the bind is forced onto loopback regardless: a non-loopback
// host is rejected and replaced with 127.0.0.1, preserving the "no 0.0.0.0
// control plane" invariant (design 005 DB4).
func (s *Server) listenAddr() string {
	host := s.Host()
	if host == "" {
		host = DefaultHost
	}
	if s.ControlEnabled() {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			host = DefaultHost
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(s.port))
}

// requireControl aborts with 403 when control mode is disabled. Applied to
// every mutating control endpoint so the read-only viewer stays read-only by
// construction.
func (s *Server) requireControl() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.ControlEnabled() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "control mode disabled — start the web server with " + envWebControl + "=1",
			})
		}
	}
}

// registerControlAPI mounts the control-dashboard HTTP API. Read/list
// endpoints are always available; mutating endpoints are gated behind
// control mode (403 otherwise).
func (s *Server) registerControlAPI(r gin.IRouter) {
	api := r.Group("/api/control")

	// --- read/list (available in both modes) ---
	api.GET("/status", s.handleControlStatus)
	api.GET("/humanoids", s.handleControlHumanoids)
	api.GET("/pairings", s.handleControlPairings)

	// --- mutating (control mode only) ---
	ctl := api.Group("", s.requireControl())
	ctl.POST("/pairings/approve", s.handlePairingApprove)
	ctl.POST("/pairings/allow", s.handlePairingAllow)
	ctl.POST("/channels/start", s.handleChannelStart)
	ctl.POST("/channels/stop", s.handleChannelStop)
	ctl.POST("/humanoids/activate", s.handleHumanoidActivate)
	ctl.POST("/humanoids/deactivate", s.handleHumanoidDeactivate)
	ctl.POST("/humanoids/governance", s.handleHumanoidGovernance)
	ctl.POST("/humanoids/reply-mode", s.handleHumanoidReplyMode)
}

// handleControlStatus reports whether control mode is enabled — the frontend
// uses it to decide whether to render mutating buttons.
func (s *Server) handleControlStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"control": s.ControlEnabled(),
		"session": s.sessionName,
	})
}

// handleControlHumanoids returns the humanoid roster (name, active, channel
// statuses) via the same MsgHumanoidList request/reply the TUI uses.
func (s *Server) handleControlHumanoids(c *gin.Context) {
	reply, err := s.pub.Request(msg.T("humanoid", "registry", "inbox"), &msg.MsgHumanoidList{}, 2*time.Second)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "humanoid registry unavailable: " + err.Error()})
		return
	}
	r, ok := reply.(*msg.MsgHumanoidListReply)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "unexpected reply type"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"humanoids": r.Humanoids})
}

// handleControlPairings returns a humanoid's pending pairing requests and
// allowlist via the MsgChannelPairList request/reply on the pairing subject.
// Query params: humanoid (required), channel (optional — empty aggregates
// every configured channel).
func (s *Server) handleControlPairings(c *gin.Context) {
	humanoid := c.Query("humanoid")
	if humanoid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing humanoid query param"})
		return
	}
	channel := c.Query("channel")
	reply, err := s.pub.Request(msg.T("humanoid", humanoid, "pairing"),
		&msg.MsgChannelPairList{HumanoidName: humanoid, Channel: channel}, 2*time.Second)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "no pairing reply (is the humanoid spawned?): " + err.Error()})
		return
	}
	lr, ok := reply.(*msg.MsgChannelPairListReply)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "unexpected reply type"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"humanoid_name": humanoid,
		"channel":       channel,
		"pending":       lr.Pending,
		"allowlist":     lr.Allowlist,
	})
}

// handlePairingApprove consumes a pending pairing code — the same
// MsgChannelPairApprove the terminal `##humanoid pair approve` publishes.
func (s *Server) handlePairingApprove(c *gin.Context) {
	var req struct {
		HumanoidName string `json:"humanoid_name"`
		Channel      string `json:"channel"`
		Code         string `json:"code"`
	}
	if c.ShouldBindJSON(&req) != nil || req.HumanoidName == "" || req.Code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "humanoid_name and code are required"})
		return
	}
	if err := s.pub.Send(msg.T("humanoid", req.HumanoidName, "pairing"), &msg.MsgChannelPairApprove{
		HumanoidName: req.HumanoidName,
		Channel:      req.Channel,
		Code:         req.Code,
	}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handlePairingAllow adds a sender directly to the allowlist, skipping the
// code flow — the same MsgChannelAllow the terminal `##humanoid allow` sends.
func (s *Server) handlePairingAllow(c *gin.Context) {
	var req struct {
		HumanoidName string `json:"humanoid_name"`
		Channel      string `json:"channel"`
		SenderID     string `json:"sender_id"`
	}
	if c.ShouldBindJSON(&req) != nil || req.HumanoidName == "" || req.SenderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "humanoid_name and sender_id are required"})
		return
	}
	if err := s.pub.Send(msg.T("humanoid", req.HumanoidName, "pairing"), &msg.MsgChannelAllow{
		HumanoidName: req.HumanoidName,
		Channel:      req.Channel,
		SenderID:     req.SenderID,
	}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// channelReq is the shared body for channel start/stop.
type channelReq struct {
	HumanoidName string `json:"humanoid_name"`
	ChannelType  string `json:"channel_type"`
}

// handleChannelStart starts a channel adapter on a humanoid — the same
// MsgHumanoidChannelStart the terminal `##humanoid channel start` sends.
func (s *Server) handleChannelStart(c *gin.Context) {
	var req channelReq
	if c.ShouldBindJSON(&req) != nil || req.HumanoidName == "" || req.ChannelType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "humanoid_name and channel_type are required"})
		return
	}
	if err := s.pub.Send(msg.T("humanoid", req.HumanoidName, "inbox"),
		&msg.MsgHumanoidChannelStart{ChannelType: req.ChannelType}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleChannelStop stops a channel adapter on a humanoid.
func (s *Server) handleChannelStop(c *gin.Context) {
	var req channelReq
	if c.ShouldBindJSON(&req) != nil || req.HumanoidName == "" || req.ChannelType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "humanoid_name and channel_type are required"})
		return
	}
	if err := s.pub.Send(msg.T("humanoid", req.HumanoidName, "inbox"),
		&msg.MsgHumanoidChannelStop{ChannelType: req.ChannelType}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleHumanoidActivate activates a humanoid via the registry inbox.
func (s *Server) handleHumanoidActivate(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if c.ShouldBindJSON(&req) != nil || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if err := s.pub.Send(msg.T("humanoid", "registry", "inbox"), &msg.MsgHumanoidActivate{Name: req.Name}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleHumanoidDeactivate deactivates a humanoid via the registry inbox.
func (s *Server) handleHumanoidDeactivate(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if c.ShouldBindJSON(&req) != nil || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if err := s.pub.Send(msg.T("humanoid", "registry", "inbox"), &msg.MsgHumanoidDeactivate{Name: req.Name}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleHumanoidGovernance flips a humanoid's governance mode (ai|human) —
// the same MsgHumanoidSetGovernance the terminal `##humanoid governance`
// sends. Note the message is humanoid-scoped (not per-channel); the spawn-time
// tool-registration caveat from humanoid.go applies (design 005 §4.4).
func (s *Server) handleHumanoidGovernance(c *gin.Context) {
	var req struct {
		HumanoidName string `json:"humanoid_name"`
		Mode         string `json:"mode"`
	}
	if c.ShouldBindJSON(&req) != nil || req.HumanoidName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "humanoid_name is required"})
		return
	}
	if req.Mode != "ai" && req.Mode != "human" {
		c.JSON(http.StatusBadRequest, gin.H{"error": `mode must be "ai" or "human"`})
		return
	}
	if err := s.pub.Send(msg.T("humanoid", req.HumanoidName, "inbox"),
		&msg.MsgHumanoidSetGovernance{Mode: req.Mode}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// handleHumanoidReplyMode flips a channel's reply mode (messages|mentions) —
// the same MsgHumanoidSetReplyMode the terminal `##humanoid reply-to` sends.
func (s *Server) handleHumanoidReplyMode(c *gin.Context) {
	var req struct {
		HumanoidName string `json:"humanoid_name"`
		ChannelType  string `json:"channel_type"`
		Mode         string `json:"mode"`
	}
	if c.ShouldBindJSON(&req) != nil || req.HumanoidName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "humanoid_name is required"})
		return
	}
	if req.Mode != "messages" && req.Mode != "mentions" {
		c.JSON(http.StatusBadRequest, gin.H{"error": `mode must be "messages" or "mentions"`})
		return
	}
	if req.ChannelType == "" {
		req.ChannelType = "slack" // same default as the terminal command
	}
	if err := s.pub.Send(msg.T("humanoid", req.HumanoidName, "inbox"),
		&msg.MsgHumanoidSetReplyMode{ChannelType: req.ChannelType, Mode: req.Mode}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Pairing event relay (humanoid → dashboard, design 005 DB2)
// ---------------------------------------------------------------------------

// humanoidFromPairingSubject extracts {name} from
// {prefix}.humanoid.{name}.pairing (names contain no dots).
func humanoidFromPairingSubject(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) < 3 || parts[len(parts)-1] != "pairing" {
		return ""
	}
	return parts[len(parts)-2]
}

// subscribePairingEvents relays humanoid-published pairing traffic into the
// WS hub so the Pairings view updates live. The humanoid.{name}.pairing
// subject is bidirectional (approver commands ride it too), so only
// humanoid→dashboard message types are forwarded:
//
//	MsgChannelPairRequest   → "pairing_request" (new pending request)
//	MsgChannelPairQR        → "pairing_qr"      (device-link payload to render)
//	MsgChannelPairStatus    → "pairing_status"  (linked / link error)
//	MsgChannelPairListReply → "pairing_list"    (fire-and-forget roster refresh)
func (s *Server) subscribePairingEvents() {
	sub, err := s.nc.Subscribe(msg.T("humanoid", "*", "pairing"), func(natMsg *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := s.codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		humanoidName := humanoidFromPairingSubject(natMsg.Subject)

		var frame map[string]interface{}
		switch m := decoded.(type) {
		case *msg.MsgChannelPairRequest:
			frame = map[string]interface{}{
				"type": "pairing_request",
				"data": m,
			}
		case *msg.MsgChannelPairQR:
			frame = map[string]interface{}{
				"type": "pairing_qr",
				"data": m,
			}
		case *msg.MsgChannelPairStatus:
			frame = map[string]interface{}{
				"type": "pairing_status",
				"data": m,
			}
		case *msg.MsgChannelPairListReply:
			frame = map[string]interface{}{
				"type": "pairing_list",
				"data": map[string]interface{}{
					"humanoid_name": humanoidName,
					"pending":       m.Pending,
					"allowlist":     m.Allowlist,
				},
			}
		default:
			// Approver→humanoid commands (Approve/Allow/PairList) also ride this
			// subject; the dashboard does not need to see them.
			return
		}
		data, err := json.Marshal(frame)
		if err != nil {
			return
		}
		s.hub.sendAll(data)
	})
	if err != nil {
		s.logger.Error("failed to subscribe to pairing events", "err", err)
		return
	}
	s.pairingSub = sub
}

// ---------------------------------------------------------------------------
// WS command dispatch (control actions)
// ---------------------------------------------------------------------------

// handleControlCommand handles the control-dashboard WS actions delegated
// from handleCommand's default case. Mutating actions are dropped (with a log
// line) unless control mode is enabled — the browser also hides the buttons,
// but the server is the enforcement point.
func (s *Server) handleControlCommand(action string, data json.RawMessage) {
	switch action {
	case "control_status":
		resp, err := json.Marshal(map[string]interface{}{
			"type": "control_status",
			"data": map[string]interface{}{
				"control": s.ControlEnabled(),
				"session": s.sessionName,
			},
		})
		if err == nil {
			s.hub.sendAll(resp)
		}

	case "pairing_list":
		// Request/reply so a fresh view can populate immediately; the reply is
		// pushed to every client as a "pairing_list" frame (the same shape the
		// fire-and-forget relay produces).
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			Channel      string `json:"channel"`
		}
		if json.Unmarshal(data, &cmd) != nil || cmd.HumanoidName == "" {
			return
		}
		reply, err := s.pub.Request(msg.T("humanoid", cmd.HumanoidName, "pairing"),
			&msg.MsgChannelPairList{HumanoidName: cmd.HumanoidName, Channel: cmd.Channel}, 2*time.Second)
		if err != nil {
			return
		}
		lr, ok := reply.(*msg.MsgChannelPairListReply)
		if !ok {
			return
		}
		resp, err := json.Marshal(map[string]interface{}{
			"type": "pairing_list",
			"data": map[string]interface{}{
				"humanoid_name": cmd.HumanoidName,
				"channel":       cmd.Channel,
				"pending":       lr.Pending,
				"allowlist":     lr.Allowlist,
			},
		})
		if err == nil {
			s.hub.sendAll(resp)
		}

	case "pairing_approve":
		if !s.ControlEnabled() {
			s.logger.Warn("web: pairing_approve rejected — control mode disabled")
			return
		}
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			Channel      string `json:"channel"`
			Code         string `json:"code"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" && cmd.Code != "" {
			_ = s.pub.Send(msg.T("humanoid", cmd.HumanoidName, "pairing"), &msg.MsgChannelPairApprove{
				HumanoidName: cmd.HumanoidName,
				Channel:      cmd.Channel,
				Code:         cmd.Code,
			})
		}

	case "pairing_allow":
		if !s.ControlEnabled() {
			s.logger.Warn("web: pairing_allow rejected — control mode disabled")
			return
		}
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			Channel      string `json:"channel"`
			SenderID     string `json:"sender_id"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" && cmd.SenderID != "" {
			_ = s.pub.Send(msg.T("humanoid", cmd.HumanoidName, "pairing"), &msg.MsgChannelAllow{
				HumanoidName: cmd.HumanoidName,
				Channel:      cmd.Channel,
				SenderID:     cmd.SenderID,
			})
		}

	case "humanoid_set_governance":
		if !s.ControlEnabled() {
			s.logger.Warn("web: humanoid_set_governance rejected — control mode disabled")
			return
		}
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			Mode         string `json:"mode"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" &&
			(cmd.Mode == "ai" || cmd.Mode == "human") {
			_ = s.pub.Send(msg.T("humanoid", cmd.HumanoidName, "inbox"),
				&msg.MsgHumanoidSetGovernance{Mode: cmd.Mode})
		}

	case "humanoid_set_reply_mode":
		if !s.ControlEnabled() {
			s.logger.Warn("web: humanoid_set_reply_mode rejected — control mode disabled")
			return
		}
		var cmd struct {
			HumanoidName string `json:"humanoid_name"`
			ChannelType  string `json:"channel_type"`
			Mode         string `json:"mode"`
		}
		if json.Unmarshal(data, &cmd) == nil && cmd.HumanoidName != "" &&
			(cmd.Mode == "messages" || cmd.Mode == "mentions") {
			if cmd.ChannelType == "" {
				cmd.ChannelType = "slack"
			}
			_ = s.pub.Send(msg.T("humanoid", cmd.HumanoidName, "inbox"),
				&msg.MsgHumanoidSetReplyMode{ChannelType: cmd.ChannelType, Mode: cmd.Mode})
		}
	}
}
