// SPDX-License-Identifier: Apache-2.0

package tools

// pane_inspect must send a request the pane's bridge can decode: the bridge
// decodes every message by TypeTag through the codec registry and drops one it
// cannot decode without replying, so an unregistered tag made every call time
// out with "pane <id> not responding (may not exist): nats: timeout".

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// fakePaneSnapshot answers pane.<id>.snapshot the way the pane's bridge does:
// it decodes the request by TypeTag, drops it when that fails, and replies with
// a MsgPaneSnapshotReply envelope to a decoded MsgGetPaneSnapshot.
func fakePaneSnapshot(t *testing.T, nc *nats.Conn, paneID string) {
	t.Helper()
	codecs := msg.DefaultCodecRegistry()
	sub, err := nc.Subscribe(msg.T("pane", paneID, "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		if _, ok := decoded.(*msg.MsgGetPaneSnapshot); !ok {
			return
		}
		payload, _ := json.Marshal(&msg.MsgPaneSnapshotReply{})
		data, _ := json.Marshal(msg.NATSEnvelope{TypeTag: msg.TagPaneSnapshotReply, Payload: payload})
		_ = m.Respond(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
}

func TestPaneInspect_PaneAnswers(t *testing.T) {
	nc := newTestNATS(t)
	fakePaneSnapshot(t, nc, "pane-1")

	out, err := NewPaneInspectTool(nc, "s").Execute(context.Background(), json.RawMessage(`{"pane_id":"pane-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.Error != "" {
		t.Fatalf("pane_inspect failed against a live pane: %s", out.Error)
	}
	if !strings.Contains(out.Content, `"snapshot"`) {
		t.Fatalf("content = %q, want the pane snapshot reply", out.Content)
	}
	if out.Metadata["pane_id"] != "pane-1" {
		t.Fatalf("metadata = %v, want pane_id pane-1", out.Metadata)
	}
}
