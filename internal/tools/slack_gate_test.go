// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The Slack draft-and-confirm gate.
//
// The first version of this feature enforced it with PROMPT TEXT: the system
// prompt told the model not to call slack_send unbidden, and slack_send set
// RequiresApproval. Neither held — a humanoid runs headless with auto-approve
// on, which skips the approval check before RequiresApproval is consulted, and
// the tool accepted a draft-less channel+body send anyway. These tests pin the
// enforcement in the tool, where no prompt and no flag can route around it.

func gatedSendTool(t *testing.T, humanMode bool) (*SlackSendTool, *channels.DraftStore) {
	t.Helper()
	drafts := channels.NewDraftStore()
	tool := NewSlackSendTool(
		channels.NewSlackAdapter(msg.ChannelConfig{}),
		drafts,
		func() bool { return humanMode },
	)
	return tool, drafts
}

func run(t *testing.T, tool *SlackSendTool, params string) *ToolOutput {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(params))
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	return out
}

func TestSlackSend_HumanMode_RefusesDirectSend(t *testing.T) {
	tool, _ := gatedSendTool(t, true)

	out := run(t, tool, `{"channel":"C123","body":"posted without asking"}`)
	if out.Error == "" {
		t.Fatal("a direct channel+body send must be refused under human governance")
	}
	if !strings.Contains(out.Error, "draft_id") {
		t.Fatalf("the refusal should tell the model what to do instead, got: %s", out.Error)
	}
}

func TestSlackSend_HumanMode_RefusesUnapprovedDraft(t *testing.T) {
	tool, drafts := gatedSendTool(t, true)
	id := drafts.Create("slack", "C123", "", "drafted but not confirmed", "")

	out := run(t, tool, `{"draft_id":"`+id+`"}`)
	if out.Error == "" {
		t.Fatal("an unapproved draft must not reach Slack — this is the whole gate")
	}
	if !strings.Contains(out.Error, "not been approved") {
		t.Fatalf("refusal should name the reason, got: %s", out.Error)
	}
}

func TestSlackSend_HumanMode_RefusesUnknownDraft(t *testing.T) {
	tool, _ := gatedSendTool(t, true)

	out := run(t, tool, `{"draft_id":"draft-does-not-exist"}`)
	if out.Error == "" {
		t.Fatal("an unknown draft id must be refused, not treated as approval")
	}
}

// TestSlackSend_HumanMode_ApprovedDraftPassesTheGate proves the gate is a gate
// and not a wall: once the owner approves, the send proceeds past the check.
// The adapter is unconnected, so it fails at the transport — which is exactly
// how we know the gate itself let it through.
func TestSlackSend_HumanMode_ApprovedDraftPassesTheGate(t *testing.T) {
	tool, drafts := gatedSendTool(t, true)
	id := drafts.Create("slack", "C123", "", "approved reply", "")
	if !drafts.Approve(id) {
		t.Fatal("Approve should find the draft it was just given")
	}

	out := run(t, tool, `{"draft_id":"`+id+`"}`)
	for _, gateMsg := range []string{"not been approved", "requires the draft_id"} {
		if strings.Contains(out.Error, gateMsg) {
			t.Fatalf("an approved draft must clear the gate, got: %s", out.Error)
		}
	}
}

// TestSlackSend_AIMode_IsUngated confirms the gate is scoped to human
// governance and does not break autonomous Slack humanoids.
func TestSlackSend_AIMode_IsUngated(t *testing.T) {
	tool, _ := gatedSendTool(t, false)

	out := run(t, tool, `{"channel":"C123","body":"autonomous reply"}`)
	if strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("ai mode must not be gated, got: %s", out.Error)
	}
}

// TestSlackSend_NilGateIsUngated covers the default construction used by
// non-humanoid callers.
func TestSlackSend_NilGateIsUngated(t *testing.T) {
	tool := NewSlackSendTool(channels.NewSlackAdapter(msg.ChannelConfig{}), channels.NewDraftStore(), nil)

	out := run(t, tool, `{"channel":"C123","body":"x"}`)
	if strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("a nil gate must not gate, got: %s", out.Error)
	}
}

// TestSlackSend_GateIsReadPerCall pins the runtime-switch behaviour: flipping
// `##humanoid governance` must take effect on the next call, not at respawn.
func TestSlackSend_GateIsReadPerCall(t *testing.T) {
	human := false
	drafts := channels.NewDraftStore()
	tool := NewSlackSendTool(
		channels.NewSlackAdapter(msg.ChannelConfig{}), drafts, func() bool { return human },
	)

	if out := run(t, tool, `{"channel":"C1","body":"x"}`); strings.Contains(out.Error, "human governance is on") {
		t.Fatal("should be ungated while ai mode is active")
	}

	human = true // ##humanoid governance <name> human

	out := run(t, tool, `{"channel":"C1","body":"x"}`)
	if out.Error == "" || !strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("switching to human mode must gate the very next call, got: %s", out.Error)
	}
}
