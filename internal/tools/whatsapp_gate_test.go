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

// The WhatsApp draft-and-confirm gate — same contract as slack_gate_test.go
// and email_gate_test.go, enforced by the shared requireApprovedDraft gate.

func gatedWhatsAppSendTool(t *testing.T, humanMode bool) (*WhatsAppSendTool, *channels.DraftStore) {
	t.Helper()
	drafts := channels.NewDraftStore()
	tool := NewWhatsAppSendTool(
		channels.NewWhatsAppAdapter(msg.ChannelConfig{}),
		drafts,
		func() bool { return humanMode },
	)
	return tool, drafts
}

func runWhatsApp(t *testing.T, tool *WhatsAppSendTool, params string) *ToolOutput {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(params))
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	return out
}

func TestWhatsAppSend_HumanMode_RefusesDirectSend(t *testing.T) {
	tool, _ := gatedWhatsAppSendTool(t, true)

	out := runWhatsApp(t, tool, `{"to":"447700900123","body":"sent without asking"}`)
	if out.Error == "" {
		t.Fatal("a direct to+body send must be refused under human governance")
	}
	if !strings.Contains(out.Error, "draft_id") {
		t.Fatalf("the refusal should tell the model what to do instead, got: %s", out.Error)
	}
}

func TestWhatsAppSend_HumanMode_RefusesUnapprovedDraft(t *testing.T) {
	tool, drafts := gatedWhatsAppSendTool(t, true)
	id := drafts.Create("whatsapp", "447700900123", "", "drafted but not confirmed", "")

	out := runWhatsApp(t, tool, `{"draft_id":"`+id+`"}`)
	if out.Error == "" {
		t.Fatal("an unapproved draft must not reach WhatsApp — this is the whole gate")
	}
	if !strings.Contains(out.Error, "not been approved") {
		t.Fatalf("refusal should name the reason, got: %s", out.Error)
	}
}

func TestWhatsAppSend_HumanMode_RefusesUnknownDraft(t *testing.T) {
	tool, _ := gatedWhatsAppSendTool(t, true)

	out := runWhatsApp(t, tool, `{"draft_id":"draft-does-not-exist"}`)
	if out.Error == "" {
		t.Fatal("an unknown draft id must be refused, not treated as approval")
	}
}

// TestWhatsAppSend_HumanMode_ApprovedDraftPassesTheGate proves the gate is a
// gate and not a wall. The adapter is unconnected, so the send fails at the
// transport ("not connected") — which is exactly how we know the gate itself
// let it through.
func TestWhatsAppSend_HumanMode_ApprovedDraftPassesTheGate(t *testing.T) {
	tool, drafts := gatedWhatsAppSendTool(t, true)
	id := drafts.Create("whatsapp", "447700900123", "", "approved reply", "")
	if !drafts.Approve(id) {
		t.Fatal("Approve should find the draft it was just given")
	}

	out := runWhatsApp(t, tool, `{"draft_id":"`+id+`"}`)
	for _, gateMsg := range []string{"not been approved", "requires the draft_id"} {
		if strings.Contains(out.Error, gateMsg) {
			t.Fatalf("an approved draft must clear the gate, got: %s", out.Error)
		}
	}
}

// TestWhatsAppSend_HumanMode_ApprovedDraftIsConsumed pins exactly-once
// semantics: the draft is deleted after the send attempt, so replaying the
// same draft_id cannot send twice.
func TestWhatsAppSend_HumanMode_ApprovedDraftIsConsumed(t *testing.T) {
	tool, drafts := gatedWhatsAppSendTool(t, true)
	id := drafts.Create("whatsapp", "447700900123", "", "approved reply", "")
	drafts.Approve(id)

	runWhatsApp(t, tool, `{"draft_id":"`+id+`"}`)

	replay := runWhatsApp(t, tool, `{"draft_id":"`+id+`"}`)
	if replay.Error == "" || !strings.Contains(replay.Error, "not found") {
		t.Fatalf("a consumed draft must not be sendable again, got: %s", replay.Error)
	}
}

func TestWhatsAppSend_AIMode_IsUngated(t *testing.T) {
	tool, _ := gatedWhatsAppSendTool(t, false)

	out := runWhatsApp(t, tool, `{"to":"447700900123","body":"autonomous reply"}`)
	if strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("ai mode must not be gated, got: %s", out.Error)
	}
}

// TestWhatsAppSend_NilGateIsUngated covers the default construction used by
// non-humanoid callers.
func TestWhatsAppSend_NilGateIsUngated(t *testing.T) {
	tool := NewWhatsAppSendTool(channels.NewWhatsAppAdapter(msg.ChannelConfig{}), channels.NewDraftStore(), nil)

	out := runWhatsApp(t, tool, `{"to":"447700900123","body":"x"}`)
	if strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("a nil gate must not gate, got: %s", out.Error)
	}
}

// TestWhatsAppSend_GateIsReadPerCall pins the runtime-switch behaviour:
// flipping `##humanoid governance` must take effect on the next call.
func TestWhatsAppSend_GateIsReadPerCall(t *testing.T) {
	human := false
	drafts := channels.NewDraftStore()
	tool := NewWhatsAppSendTool(
		channels.NewWhatsAppAdapter(msg.ChannelConfig{}), drafts, func() bool { return human },
	)

	if out := runWhatsApp(t, tool, `{"to":"447700900123","body":"x"}`); strings.Contains(out.Error, "human governance is on") {
		t.Fatal("should be ungated while ai mode is active")
	}

	human = true // ##humanoid governance <name> human

	out := runWhatsApp(t, tool, `{"to":"447700900123","body":"x"}`)
	if out.Error == "" || !strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("switching to human mode must gate the very next call, got: %s", out.Error)
	}
}

// TestWhatsAppSend_GateHoldsUnderHeadlessAutoApprove — see the email twin for
// why: auto-approve reaches Execute with no human in the loop, and the gate
// must still refuse.
func TestWhatsAppSend_GateHoldsUnderHeadlessAutoApprove(t *testing.T) {
	tool, drafts := gatedWhatsAppSendTool(t, true)
	if !tool.RequiresApproval(nil) {
		t.Fatal("precondition: whatsapp_send declares RequiresApproval — the point is that this flag alone is not enforcement")
	}
	id := drafts.Create("whatsapp", "447700900123", "", "unapproved", "")

	out := runWhatsApp(t, tool, `{"draft_id":"`+id+`"}`)
	if out.Error == "" {
		t.Fatal("auto-approved execution must NOT bypass the owner-approval gate")
	}
	if !strings.Contains(out.Error, "not been approved") {
		t.Fatalf("the refusal must come from the approval gate, not the transport, got: %s", out.Error)
	}
}

// TestWhatsAppSendTemplate_HumanMode_Refused closes the side door: template
// sends carry model-chosen parameter text but can never come from a
// reviewable draft, so under human governance the tool refuses outright.
func TestWhatsAppSendTemplate_HumanMode_Refused(t *testing.T) {
	tool := NewWhatsAppSendTemplateTool(
		channels.NewWhatsAppAdapter(msg.ChannelConfig{}),
		func() bool { return true },
	)

	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"to":"447700900123","template_name":"hello_world","params":["smuggled text"]}`))
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	if out.Error == "" || !strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("whatsapp_send_template must refuse under human governance, got: %s", out.Error)
	}
}

// TestWhatsAppSendTemplate_AIMode_IsUngated keeps templates working for
// autonomous humanoids (they fail at the unconnected transport, past any
// governance refusal).
func TestWhatsAppSendTemplate_AIMode_IsUngated(t *testing.T) {
	tool := NewWhatsAppSendTemplateTool(
		channels.NewWhatsAppAdapter(msg.ChannelConfig{}),
		func() bool { return false },
	)

	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"to":"447700900123","template_name":"hello_world"}`))
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	if strings.Contains(out.Error, "human governance is on") {
		t.Fatalf("ai mode must not be gated, got: %s", out.Error)
	}
}
