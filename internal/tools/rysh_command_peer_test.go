// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestRyshCommandPeerWaitsForReceiptAndReportsRefusal(t *testing.T) {
	nc := newTestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	sub, err := nc.Subscribe(msg.T("ws", "inbox"), func(m *nats.Msg) {
		var envelope msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &envelope); err != nil {
			t.Error(err)
			return
		}
		var command msg.MsgCLIPeerCommand
		if err := json.Unmarshal(envelope.Payload, &command); err != nil {
			t.Error(err)
			return
		}
		if command.AsPaneID != "worker-a" {
			t.Errorf("wrong sender: %+v", command)
		}
		time.Sleep(750 * time.Millisecond) // beyond the legacy collector's quiet window
		response := &msg.MsgCLIResponse{OK: true, Output: "stored sequence=1"}
		if len(command.Args) > 0 && command.Args[0] == "tail" {
			response = &msg.MsgCLIResponse{Error: "membership revoked"}
		}
		re := msg.RequestEnvelope{ReplyTo: envelope.ReplyTo, NC: nc, Codecs: codecs}
		if err := re.Reply(response); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	nc.Flush()
	tool := NewRyshCommandTool(nc, "session-a", "worker-a")
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"peer post hello"}`))
	if err != nil || output.Content != "stored sequence=1" {
		t.Fatalf("missing receipt: %+v %v", output, err)
	}
	output, err = tool.Execute(context.Background(), json.RawMessage(`{"command":"peer tail"}`))
	if err != nil || !strings.Contains(output.Error, "membership revoked") {
		t.Fatalf("missing refusal: %+v %v", output, err)
	}
	if tool.RequiresApproval(json.RawMessage(`{"command":"peer tail --json"}`)) {
		t.Fatal("peer read needs approval")
	}
	if !tool.RequiresApproval(json.RawMessage(`{"command":"peer post hi"}`)) {
		t.Fatal("peer write bypasses approval")
	}
}
