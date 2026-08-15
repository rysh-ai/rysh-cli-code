// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestHistoryAppendSignalsLayoutDirty verifies that appending a command-history
// entry to a pane publishes a layoutDirty signal.
//
// Command history lives in the layout-only workspace snapshot (there is no
// separate content stream for it). Snapshot-driven clients — the desktop app's
// ?stream=1 WebSocket channel and the TUI's event-driven layout fetch — only
// re-fetch that snapshot on ws.layoutDirty. Before the fix, a history append
// set kvDirty but signalled nothing, so the desktop app never saw new entries
// (up/down-arrow recall stayed frozen at whatever the last unrelated layout
// event captured) and the TUI lagged up to a full reconcile tick.
func TestHistoryAppendSignalsLayoutDirty(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	sub, err := nc.SubscribeSync(msg.T("ws", "layoutDirty"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	p := &PaneActor{
		id:            "hist-dirty-pane",
		pub:           pub,
		conversations: make(map[msg.ConversationType][]*msg.ConversationMessage),
		convHistories: make(map[msg.ConversationType][]*msg.ConversationMessage),
		activeTurnIDs: make(map[msg.ConversationType]string),
	}

	p.handleConversationHistoryAppend(&msg.ConversationMessage{
		TurnID:           "turn-1",
		TurnType:         msg.TurnQuestion,
		ConversationType: msg.ConvShell,
		InputType:        msg.InputShell,
		MessageSource:    msg.SourceHuman,
		Content:          "echo hello-history",
	})

	// The legacy shell history array (what the snapshot / up-arrow recall
	// reads) must be updated…
	if len(p.shellHistory) != 1 || p.shellHistory[0] != "echo hello-history" {
		t.Fatalf("shellHistory not appended: %q", p.shellHistory)
	}

	// …and a layoutDirty signal must be published so snapshot-driven clients
	// re-fetch the layout snapshot that carries the history.
	m, err := sub.NextMsg(time.Second)
	if err != nil {
		t.Fatalf("expected layoutDirty signal after history append, got none: %v", err)
	}
	var env msg.NATSEnvelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	decoded, err := codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		t.Fatalf("decode %s: %v", env.TypeTag, err)
	}
	if _, ok := decoded.(*msg.MsgLayoutDirty); !ok {
		t.Fatalf("expected MsgLayoutDirty on ws.layoutDirty, got %T", decoded)
	}
}
