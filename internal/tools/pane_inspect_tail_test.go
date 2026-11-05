// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// N2: pane_inspect answers with a compact view of the pane: who it is, its
// state, and the LAST tail_lines lines of its current output. The rest of the
// snapshot (other modes' output, histories, conversations) is not the
// question an agent asked, and it never reaches the model.
func fakePaneSnapshotWith(t *testing.T, nc *nats.Conn, paneID string, snap domain.PaneSnapshot) {
	t.Helper()
	sub, err := nc.Subscribe(msg.T("pane", paneID, "snapshot"), func(m *nats.Msg) {
		payload, _ := json.Marshal(&msg.MsgPaneSnapshotReply{Snapshot: snap})
		data, _ := json.Marshal(msg.NATSEnvelope{TypeTag: msg.TagPaneSnapshotReply, Payload: payload})
		_ = m.Respond(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
}

func TestPaneInspect_ReturnsTheTailOfTheOutputOnly(t *testing.T) {
	nc := newTestNATS(t)
	lines := make([]string, 120)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%03d", i+1)
	}
	fakePaneSnapshotWith(t, nc, "pane-1", domain.PaneSnapshot{
		ID: "pane-1", Title: "build", Mode: "shell", Status: "running shell", LastCommand: "make test",
		Program:       "make",
		Output:        strings.Join(lines, "\n") + "\n",
		ShellHistory:  []string{"HISTORY-MARKER-cmd"},
		ChatOutput:    "OTHER-MODE-MARKER",
		Conversations: map[string][]domain.ConversationMessageSnapshot{"ai": {{Content: "CONV-MARKER"}}},
	})
	tool := NewPaneInspectTool(nc, "s")
	call := func(args string) map[string]any {
		t.Helper()
		out, err := tool.Execute(context.Background(), json.RawMessage(args))
		if err != nil || out.Error != "" {
			t.Fatalf("%s: err=%v out=%v", args, err, out)
		}
		for _, leak := range []string{"HISTORY-MARKER", "OTHER-MODE-MARKER", "CONV-MARKER"} {
			if strings.Contains(out.Content, leak) {
				t.Fatalf("%s: the answer carries %s", args, leak)
			}
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(out.Content), &v); err != nil {
			t.Fatalf("%s: content is not JSON: %v\n%s", args, err, out.Content)
		}
		return v
	}

	v := call(`{"pane_id":"pane-1","tail_lines":10}`)
	tail, _ := v["output_tail"].(string)
	if got := strings.Split(strings.TrimSuffix(tail, "\n"), "\n"); len(got) != 10 || got[0] != "line-111" || got[9] != "line-120" {
		t.Fatalf("tail_lines 10: got %d lines (%q..)", len(got), got[0])
	}
	if v["truncated"] != true || v["pane_id"] != "pane-1" || v["mode"] != "shell" || v["status"] != "running shell" ||
		v["title"] != "build" || v["last_command"] != "make test" || v["program"] != "make" {
		t.Fatalf("compact view: %v", v)
	}
	// Default 50; at most 500; a short output is whole and not truncated.
	if got := strings.Count(call(`{"pane_id":"pane-1"}`)["output_tail"].(string), "\n"); got != 50 {
		t.Fatalf("default tail: %d lines, want 50", got)
	}
	whole := call(`{"pane_id":"pane-1","tail_lines":5000}`)
	if got := strings.Count(whole["output_tail"].(string), "\n"); got != 120 || whole["truncated"] != false {
		t.Fatalf("tail_lines above the output: %d lines, truncated=%v", got, whole["truncated"])
	}
}
