// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The desktop/web client toggles the tab bar through the same typed message the
// TUI's ctrl+t v sends (MsgSetTabBarOrientation), rather than by injecting
// `##tab orientation` text into whichever pane happens to be active. These
// tests pin that wiring: the mapping from the wire action to the message, and
// the refusal to act on anything else.

// setTabOrientation runs one command through handleCommand and returns the
// envelope it published on ws.inbox, or nil if it published nothing.
func setTabOrientation(t *testing.T, s *Server, sub *nats.Subscription, body string) *msg.NATSEnvelope {
	t.Helper()
	s.handleCommand("set_tab_orientation", json.RawMessage(body))
	m, err := sub.NextMsg(time.Second)
	if err != nil {
		return nil
	}
	var env msg.NATSEnvelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return &env
}

func TestSetTabOrientationCommand(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	sub, err := nc.SubscribeSync(msg.T("ws", "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	for _, tc := range []struct {
		orientation  string
		wantVertical bool
		wantToggle   bool
	}{
		{orientation: "vertical", wantVertical: true},
		{orientation: "horizontal"},
		{orientation: "toggle", wantToggle: true},
	} {
		t.Run(tc.orientation, func(t *testing.T) {
			env := setTabOrientation(t, s, sub, `{"orientation":"`+tc.orientation+`"}`)
			if env == nil {
				t.Fatalf("orientation %q published nothing to ws.inbox", tc.orientation)
			}
			if env.TypeTag != msg.TagSetTabBarOrient {
				t.Fatalf("published %s, want %s", env.TypeTag, msg.TagSetTabBarOrient)
			}
			var got msg.MsgSetTabBarOrientation
			if err := json.Unmarshal(env.Payload, &got); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if got.Vertical != tc.wantVertical || got.Toggle != tc.wantToggle {
				t.Errorf("orientation %q → {Vertical:%v Toggle:%v}, want {Vertical:%v Toggle:%v}",
					tc.orientation, got.Vertical, got.Toggle, tc.wantVertical, tc.wantToggle)
			}
		})
	}
}

// An unknown or absent orientation must publish nothing. Defaulting it to
// toggle would make a client that asked for a specific orientation — and was
// misheard — flip the bar away from what the user picked.
func TestSetTabOrientationIgnoresUnknownValues(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	sub, err := nc.SubscribeSync(msg.T("ws", "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	for _, body := range []string{`{}`, `{"orientation":""}`, `{"orientation":"sideways"}`, `not json`} {
		if env := setTabOrientation(t, s, sub, body); env != nil {
			t.Errorf("body %s published %s, want nothing", body, env.TypeTag)
		}
	}
}
