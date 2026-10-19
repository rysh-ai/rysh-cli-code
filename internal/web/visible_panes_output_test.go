// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The visible-pane filter covered only pane_vt; every pane's pane_output still
// streamed to every browser — busy agent panes on other tabs included — and the
// page buffered and re-rendered all of it. Output for panes a client is not
// showing is now skipped (chat excepted), and a pane that comes on screen is
// caught up with its current content.

type outputMsg struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// drain collects the client's messages for d, by type.
func drain(c *wsClient, d time.Duration) map[string][]json.RawMessage {
	out := map[string][]json.RawMessage{}
	deadline := time.After(d)
	for {
		select {
		case raw := <-c.send:
			var m outputMsg
			if json.Unmarshal(raw, &m) == nil {
				out[m.Type] = append(out[m.Type], m.Data)
			}
		case <-deadline:
			return out
		}
	}
}

func publishOutput(t *testing.T, pub *msg.NATSPublisher, paneID, mode, text string) {
	t.Helper()
	var err error
	switch mode {
	case "chat":
		err = pub.Send(msg.T("pane", paneID, "output", "chat"), &msg.MsgPaneChatOutputAppend{Text: text})
	default:
		err = pub.Send(msg.T("pane", paneID, "output", "shell"), &msg.MsgPaneOutputAppend{Text: text})
	}
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func outputServer(t *testing.T) (*Server, *nats.Conn, *msg.NATSPublisher, context.CancelFunc) {
	s, nc, _ := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.hub = newHub()
	go s.hub.run(ctx)
	s.subscribePaneOutput()
	return s, nc, msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry()), cancel
}

func TestPaneOutputGoesOnlyToClientsShowingThePane(t *testing.T) {
	s, _, pub, cancel := outputServer(t)
	defer cancel()

	all := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	picky := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	picky.vt.set([]string{"other"})
	s.hub.register <- all
	s.hub.register <- picky

	publishOutput(t, pub, "busy", "shell", "build log line\n")
	publishOutput(t, pub, "busy", "chat", "model says hi")

	if got := len(drain(all, 300*time.Millisecond)["pane_output"]); got != 2 {
		t.Errorf("unfiltered client got %d pane_output, want 2 (older bundles keep everything)", got)
	}
	got := drain(picky, 300*time.Millisecond)["pane_output"]
	if len(got) != 1 || !json.Valid(got[0]) {
		t.Fatalf("client not showing 'busy' got %d pane_output, want only the chat chunk", len(got))
	}
	var d struct{ Mode string }
	_ = json.Unmarshal(got[0], &d)
	if d.Mode != "chat" {
		t.Errorf("the chunk that got through was mode %q, want chat", d.Mode)
	}
}

func TestPaneComingOnScreenIsCaughtUp(t *testing.T) {
	s, nc, _, cancel := outputServer(t)
	defer cancel()

	// The pane actor stand-in: answers a snapshot request with its content.
	codecs := msg.DefaultCodecRegistry()
	sub, err := nc.Subscribe(msg.T("pane", "busy", "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msg.MsgPaneSnapshotReply{Snapshot: domain.PaneSnapshot{ID: "busy", Output: "everything so far"}})
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	_ = nc.Flush()

	c := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	c.vt.set([]string{"other"})
	s.hub.register <- c

	s.handleVisiblePanes(c, json.RawMessage(`{"pane_ids":["other","busy"]}`))
	got := drain(c, 500*time.Millisecond)["pane_content"]
	if len(got) != 1 {
		t.Fatalf("pane coming on screen: got %d pane_content messages, want 1", len(got))
	}
	var body struct {
		Panes []struct {
			PaneID string `json:"pane_id"`
			Output string `json:"output"`
		} `json:"panes"`
	}
	_ = json.Unmarshal(got[0], &body)
	if len(body.Panes) != 1 || body.Panes[0].PaneID != "busy" || body.Panes[0].Output != "everything so far" {
		t.Fatalf("catch-up carried %+v, want pane 'busy' with its current output", body.Panes)
	}

	// Re-declaring the same set is not a change: no second catch-up.
	s.handleVisiblePanes(c, json.RawMessage(`{"pane_ids":["other","busy"]}`))
	if n := len(drain(c, 300*time.Millisecond)["pane_content"]); n != 0 {
		t.Errorf("unchanged visible set sent %d more catch-ups, want 0", n)
	}
}
