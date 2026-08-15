// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// startInProcessNATS spins up an embedded, non-listening NATS server and
// returns a connected client (mirrors internal/channels/pairing_test.go's
// harness, minus JetStream — the control API needs only pub/sub).
func startInProcessNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{
		Host:       "127.0.0.1",
		Port:       -1,
		NoLog:      true,
		NoSigs:     true,
		DontListen: true,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Skipf("embedded NATS unavailable: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Skip("nats server not ready")
	}
	t.Cleanup(ns.Shutdown)

	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		ns.Shutdown()
		t.Fatalf("connect in-process nats: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// newControlTestServer builds a Server over an in-process NATS bus plus a gin
// router with only the control API mounted (the full Start() path — static
// assets, snapshot polling, WS hub — is not needed to exercise the HTTP API).
func newControlTestServer(t *testing.T) (*Server, *nats.Conn, *gin.Engine) {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	s := NewServer(23232, "test-session", pub, nc, pub.Codecs())
	s.SetControl(false) // independent of any ambient RYSH_WEB_CONTROL

	gin.SetMode(gin.TestMode)
	r := gin.New()
	s.registerControlAPI(r)
	return s, nc, r
}

func postJSON(t *testing.T, r *gin.Engine, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// nextEnvelope waits for one message on sub and decodes its NATSEnvelope.
func nextEnvelope(t *testing.T, sub *nats.Subscription) msg.NATSEnvelope {
	t.Helper()
	m, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected a published message: %v", err)
	}
	var env msg.NATSEnvelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env
}

// TestControlGating403 asserts every mutating control endpoint returns 403
// when control mode is disabled (DB4), and that /status reports the mode.
func TestControlGating403(t *testing.T) {
	s, nc, r := newControlTestServer(t)

	// Nothing must reach the bus while gated.
	pairSub, err := nc.SubscribeSync(msg.T("humanoid", "gate-bot", "pairing"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	inboxSub, err := nc.SubscribeSync(msg.T("humanoid", "gate-bot", "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	mutating := []struct {
		path string
		body interface{}
	}{
		{"/api/control/pairings/approve", map[string]string{"humanoid_name": "gate-bot", "code": "ABC123"}},
		{"/api/control/pairings/allow", map[string]string{"humanoid_name": "gate-bot", "sender_id": "U1"}},
		{"/api/control/channels/start", map[string]string{"humanoid_name": "gate-bot", "channel_type": "slack"}},
		{"/api/control/channels/stop", map[string]string{"humanoid_name": "gate-bot", "channel_type": "slack"}},
		{"/api/control/humanoids/activate", map[string]string{"name": "gate-bot"}},
		{"/api/control/humanoids/deactivate", map[string]string{"name": "gate-bot"}},
		{"/api/control/humanoids/governance", map[string]string{"humanoid_name": "gate-bot", "mode": "human"}},
		{"/api/control/humanoids/reply-mode", map[string]string{"humanoid_name": "gate-bot", "mode": "mentions"}},
	}
	for _, m := range mutating {
		if w := postJSON(t, r, m.path, m.body); w.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", m.path, w.Code)
		}
	}

	// Status stays readable and reports control=false.
	req := httptest.NewRequest(http.MethodGet, "/api/control/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var status struct {
		Control bool   `json:"control"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Control || status.Session != "test-session" {
		t.Fatalf("unexpected status: %+v", status)
	}

	// Gated WS actions must also drop without publishing.
	s.handleControlCommand("pairing_approve", json.RawMessage(`{"humanoid_name":"gate-bot","code":"ABC123"}`))
	s.handleControlCommand("humanoid_set_governance", json.RawMessage(`{"humanoid_name":"gate-bot","mode":"human"}`))

	if m, err := pairSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Fatalf("gated endpoint published to pairing subject: %s", m.Data)
	}
	if m, err := inboxSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Fatalf("gated endpoint published to inbox subject: %s", m.Data)
	}
}

// TestControlApprovePublishes asserts the approve endpoint publishes a
// correctly-coded MsgChannelPairApprove on humanoid.{name}.pairing — the same
// subject and message the terminal `##humanoid pair approve` drives.
func TestControlApprovePublishes(t *testing.T) {
	s, nc, r := newControlTestServer(t)
	s.SetControl(true)

	sub, err := nc.SubscribeSync(msg.T("humanoid", "gate-bot", "pairing"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	w := postJSON(t, r, "/api/control/pairings/approve",
		map[string]string{"humanoid_name": "gate-bot", "channel": "slack", "code": "4F2A9C"})
	if w.Code != http.StatusOK {
		t.Fatalf("approve: got %d body=%s, want 200", w.Code, w.Body.String())
	}

	env := nextEnvelope(t, sub)
	if env.TypeTag != msg.TagChannelPairApprove {
		t.Fatalf("got type %q, want %q", env.TypeTag, msg.TagChannelPairApprove)
	}
	var approve msg.MsgChannelPairApprove
	if err := json.Unmarshal(env.Payload, &approve); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if approve.HumanoidName != "gate-bot" || approve.Channel != "slack" || approve.Code != "4F2A9C" {
		t.Fatalf("unexpected approve payload: %+v", approve)
	}
}

// TestControlAllowPublishes asserts the direct-allow endpoint publishes a
// MsgChannelAllow on the pairing subject.
func TestControlAllowPublishes(t *testing.T) {
	s, nc, r := newControlTestServer(t)
	s.SetControl(true)

	sub, err := nc.SubscribeSync(msg.T("humanoid", "gate-bot", "pairing"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	w := postJSON(t, r, "/api/control/pairings/allow",
		map[string]string{"humanoid_name": "gate-bot", "channel": "whatsapp", "sender_id": "+15550142"})
	if w.Code != http.StatusOK {
		t.Fatalf("allow: got %d body=%s, want 200", w.Code, w.Body.String())
	}

	env := nextEnvelope(t, sub)
	if env.TypeTag != msg.TagChannelAllow {
		t.Fatalf("got type %q, want %q", env.TypeTag, msg.TagChannelAllow)
	}
	var allow msg.MsgChannelAllow
	if err := json.Unmarshal(env.Payload, &allow); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if allow.HumanoidName != "gate-bot" || allow.Channel != "whatsapp" || allow.SenderID != "+15550142" {
		t.Fatalf("unexpected allow payload: %+v", allow)
	}
}

// TestControlPairListRoundTrip runs the pair-list endpoint against a fake
// humanoid responder: the endpoint issues a MsgChannelPairList request on the
// pairing subject and returns the responder's MsgChannelPairListReply as JSON.
func TestControlPairListRoundTrip(t *testing.T) {
	_, nc, r := newControlTestServer(t) // read endpoint — no control mode needed

	// Fake humanoid: answer MsgChannelPairList requests on the pairing subject.
	_, err := nc.Subscribe(msg.T("humanoid", "gate-bot", "pairing"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil || env.TypeTag != msg.TagChannelPairList || env.ReplyTo == "" {
			return
		}
		reply := msg.MsgChannelPairListReply{
			Pending: []msg.PendingPair{{
				Code:       "4F2A9C",
				SenderID:   "+15550142",
				SenderName: "Kay",
				Channel:    "whatsapp",
				FirstMsg:   "hello?",
				CreatedAt:  time.Now().Unix(),
				ExpiresAt:  time.Now().Add(time.Hour).Unix(),
			}},
			Allowlist: []string{"whatsapp:+15550100"},
		}
		payload, _ := json.Marshal(reply)
		out, _ := json.Marshal(msg.NATSEnvelope{TypeTag: msg.TagChannelPairListReply, Payload: payload})
		_ = nc.Publish(env.ReplyTo, out)
	})
	if err != nil {
		t.Fatalf("subscribe responder: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/control/pairings?humanoid=gate-bot&channel=whatsapp", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("pairings: got %d body=%s, want 200", w.Code, w.Body.String())
	}

	var got struct {
		HumanoidName string            `json:"humanoid_name"`
		Channel      string            `json:"channel"`
		Pending      []msg.PendingPair `json:"pending"`
		Allowlist    []string          `json:"allowlist"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if got.HumanoidName != "gate-bot" || got.Channel != "whatsapp" {
		t.Fatalf("unexpected reply scope: %+v", got)
	}
	if len(got.Pending) != 1 || got.Pending[0].Code != "4F2A9C" || got.Pending[0].SenderID != "+15550142" {
		t.Fatalf("unexpected pending: %+v", got.Pending)
	}
	if len(got.Allowlist) != 1 || got.Allowlist[0] != "whatsapp:+15550100" {
		t.Fatalf("unexpected allowlist: %+v", got.Allowlist)
	}

	// Missing humanoid param is a 400, not a bus round-trip.
	req = httptest.NewRequest(http.MethodGet, "/api/control/pairings", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("pairings without humanoid: got %d, want 400", w.Code)
	}
}

// TestControlChannelStartStop asserts the channel start/stop endpoints emit
// MsgHumanoidChannelStart/Stop on humanoid.{name}.inbox.
func TestControlChannelStartStop(t *testing.T) {
	s, nc, r := newControlTestServer(t)
	s.SetControl(true)

	sub, err := nc.SubscribeSync(msg.T("humanoid", "support-bot", "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	w := postJSON(t, r, "/api/control/channels/start",
		map[string]string{"humanoid_name": "support-bot", "channel_type": "slack"})
	if w.Code != http.StatusOK {
		t.Fatalf("start: got %d, want 200", w.Code)
	}
	env := nextEnvelope(t, sub)
	if env.TypeTag != msg.TagHumanoidChannelStart {
		t.Fatalf("got type %q, want %q", env.TypeTag, msg.TagHumanoidChannelStart)
	}
	var start msg.MsgHumanoidChannelStart
	if err := json.Unmarshal(env.Payload, &start); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if start.ChannelType != "slack" {
		t.Fatalf("unexpected start payload: %+v", start)
	}

	w = postJSON(t, r, "/api/control/channels/stop",
		map[string]string{"humanoid_name": "support-bot", "channel_type": "email"})
	if w.Code != http.StatusOK {
		t.Fatalf("stop: got %d, want 200", w.Code)
	}
	env = nextEnvelope(t, sub)
	if env.TypeTag != msg.TagHumanoidChannelStop {
		t.Fatalf("got type %q, want %q", env.TypeTag, msg.TagHumanoidChannelStop)
	}
	var stop msg.MsgHumanoidChannelStop
	if err := json.Unmarshal(env.Payload, &stop); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if stop.ChannelType != "email" {
		t.Fatalf("unexpected stop payload: %+v", stop)
	}
}

// TestControlGovernanceAndReplyMode asserts the governance and reply-mode
// endpoints emit the same messages the terminal commands send, and validate
// modes.
func TestControlGovernanceAndReplyMode(t *testing.T) {
	s, nc, r := newControlTestServer(t)
	s.SetControl(true)

	sub, err := nc.SubscribeSync(msg.T("humanoid", "support-bot", "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	w := postJSON(t, r, "/api/control/humanoids/governance",
		map[string]string{"humanoid_name": "support-bot", "mode": "human"})
	if w.Code != http.StatusOK {
		t.Fatalf("governance: got %d, want 200", w.Code)
	}
	env := nextEnvelope(t, sub)
	if env.TypeTag != msg.TagHumanoidSetGovernance {
		t.Fatalf("got type %q, want %q", env.TypeTag, msg.TagHumanoidSetGovernance)
	}
	var gov msg.MsgHumanoidSetGovernance
	if err := json.Unmarshal(env.Payload, &gov); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if gov.Mode != "human" {
		t.Fatalf("unexpected governance payload: %+v", gov)
	}

	// Invalid mode → 400, nothing published.
	w = postJSON(t, r, "/api/control/humanoids/governance",
		map[string]string{"humanoid_name": "support-bot", "mode": "autopilot"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad governance mode: got %d, want 400", w.Code)
	}

	w = postJSON(t, r, "/api/control/humanoids/reply-mode",
		map[string]string{"humanoid_name": "support-bot", "channel_type": "slack", "mode": "mentions"})
	if w.Code != http.StatusOK {
		t.Fatalf("reply-mode: got %d, want 200", w.Code)
	}
	env = nextEnvelope(t, sub)
	if env.TypeTag != msg.TagHumanoidSetReplyMode {
		t.Fatalf("got type %q, want %q", env.TypeTag, msg.TagHumanoidSetReplyMode)
	}
	var rm msg.MsgHumanoidSetReplyMode
	if err := json.Unmarshal(env.Payload, &rm); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if rm.ChannelType != "slack" || rm.Mode != "mentions" {
		t.Fatalf("unexpected reply-mode payload: %+v", rm)
	}

	// No stray extra messages.
	if m, err := sub.NextMsg(150 * time.Millisecond); err == nil {
		t.Fatalf("unexpected extra publish: %s", m.Data)
	}
}

// TestControlListenAddrLoopback asserts control mode forces a loopback bind
// (DB4). The read-only viewer now defaults to loopback too, so both modes are
// private unless an explicit --bind widens the read-only surface; see
// TestListenAddrHost in bind_test.go for the explicit-bind matrix.
func TestControlListenAddrLoopback(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	s := NewServer(23232, "test-session", pub, nc, pub.Codecs())

	s.SetControl(false)
	if got := s.listenAddr(); got != "127.0.0.1:23232" {
		t.Fatalf("read-only listen addr: got %q, want %q", got, "127.0.0.1:23232")
	}
	s.SetControl(true)
	if got := s.listenAddr(); got != "127.0.0.1:23232" {
		t.Fatalf("control listen addr: got %q, want %q", got, "127.0.0.1:23232")
	}
}

// TestHumanoidFromPairingSubject covers subject parsing with and without a
// session prefix.
func TestHumanoidFromPairingSubject(t *testing.T) {
	cases := map[string]string{
		"humanoid.support-bot.pairing":         "support-bot",
		"acme.sess1.humanoid.gate-bot.pairing": "gate-bot",
		"humanoid.registry.inbox":              "",
		"pairing":                              "",
		"humanoid.x.pairing.extra":             "",
	}
	for subject, want := range cases {
		if got := humanoidFromPairingSubject(subject); got != want {
			t.Errorf("humanoidFromPairingSubject(%q) = %q, want %q", subject, got, want)
		}
	}
}

// TestControlForcesLoopbackOverConfiguredHost covers the interaction between
// the configurable bind host (c2090f7) and control mode (DB4/R2), which arrived
// on separate branches: an operator who binds the viewer to all interfaces and
// then enables control must NOT get a control plane exposed off-host. Control
// mode wins and downgrades the bind to loopback.
//
// The roadmap's first non-goal is "no always-on 0.0.0.0 control plane"; this is
// the test that keeps that structurally true rather than merely intended.
func TestControlForcesLoopbackOverConfiguredHost(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	for _, host := range []string{"0.0.0.0", "192.168.1.10", "::"} {
		s := NewServer(23232, "test-session", pub, nc, pub.Codecs())
		s.SetHost(host)

		// Read-only: the operator's chosen host is honoured.
		s.SetControl(false)
		if got := s.listenAddr(); got == "127.0.0.1:23232" {
			t.Errorf("host %q read-only: bind was downgraded unnecessarily (%q)", host, got)
		}

		// Control on: forced to loopback regardless of the configured host.
		s.SetControl(true)
		if got := s.listenAddr(); got != "127.0.0.1:23232" {
			t.Errorf("host %q + control: got %q, want 127.0.0.1:23232 — the control plane must never bind off-host", host, got)
		}
	}

	// A loopback host the operator chose explicitly is preserved as-is.
	s := NewServer(23232, "test-session", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetControl(true)
	if got := s.listenAddr(); got != "127.0.0.1:23232" {
		t.Errorf("explicit loopback host: got %q", got)
	}
}
