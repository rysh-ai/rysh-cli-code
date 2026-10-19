// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// healthJSON starts a control-posture server (no login: /health is public
// either way) and returns its /health body.
func healthJSON(t *testing.T, setup func(*Server)) map[string]any {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	port := freePort(t)
	s := NewServer(port, "health-session", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	if setup != nil {
		setup(s)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	waitForHTTP(t, url)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	var h map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	return h
}

// Design 028 §9 limit 7: /health shows whose NATS server a guest daemon is
// writing into, and says so loudly when it belongs to another session.
func TestHealthShowsGuestOfForAGuest(t *testing.T) {
	g := bus.GuestOf{ServerID: "NSRV", Port: 4222, HostSession: "test",
		HostWorkspace: "/stray", HostPID: 77, Mismatch: true, Warning: "GUEST of another session"}
	h := healthJSON(t, func(s *Server) {
		s.SetGuestOf(func() any {
			return g.Public()
		})
	})
	got, ok := h["guest_of"].(map[string]any)
	if !ok {
		t.Fatalf("/health has no guest_of object: %v", h)
	}
	if got["server_id"] != "NSRV" || got["host_session"] != "test" || got["mismatch"] != true || got["warning"] == "" {
		t.Errorf("guest_of = %v; want server_id, host_session, mismatch and the warning", got)
	}
	if _, leaked := got["host_workspace"]; leaked && got["host_workspace"] != "" {
		t.Errorf("/health is public; it must not carry the host's filesystem path: %v", got)
	}
	if h["status"] != "ok" {
		t.Errorf("status = %v; guest mode is not an outage", h["status"])
	}
}

func TestHealthHasNoGuestOfForANonGuest(t *testing.T) {
	for name, setup := range map[string]func(*Server){
		"never wired":        nil,
		"wired, not a guest": func(s *Server) { s.SetGuestOf(func() any { return nil }) },
	} {
		h := healthJSON(t, setup)
		if v, ok := h["guest_of"]; ok {
			t.Errorf("%s: /health guest_of = %v; want it absent", name, v)
		}
	}
}
