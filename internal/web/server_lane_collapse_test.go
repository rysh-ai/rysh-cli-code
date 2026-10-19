// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 053: the desktop/web client folds and opens lanes through the same
// typed message the TUI's layout keys and strip click send (MsgLaneCollapse).
func TestLaneCollapseCommand(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	sub, err := nc.SubscribeSync(msg.T("ws", "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	for _, tc := range []struct{ verb, lane string }{
		{"collapse", "lane-2"},
		{"expand", "all"},
		{"toggle", ""},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"verb": tc.verb, "lane": tc.lane})
			s.handleCommand("lane_collapse", body)
			m, err := sub.NextMsg(time.Second)
			if err != nil {
				t.Fatalf("%s published nothing: %v", tc.verb, err)
			}
			var env msg.NATSEnvelope
			if err := json.Unmarshal(m.Data, &env); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			if env.TypeTag != msg.TagLaneCollapse {
				t.Fatalf("published %s, want %s", env.TypeTag, msg.TagLaneCollapse)
			}
			var got msg.MsgLaneCollapse
			if err := json.Unmarshal(env.Payload, &got); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if got.Verb != tc.verb || got.Lane != tc.lane {
				t.Fatalf("got %+v, want verb=%q lane=%q", got, tc.verb, tc.lane)
			}
		})
	}

	t.Run("unknown verb is dropped", func(t *testing.T) {
		s.handleCommand("lane_collapse", json.RawMessage(`{"verb":"fold","lane":"1"}`))
		if m, err := sub.NextMsg(200 * time.Millisecond); err == nil {
			t.Fatalf("unknown verb published %s", m.Data)
		}
	})
}
