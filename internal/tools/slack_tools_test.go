package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestSlackDraftTool(t *testing.T) {
	drafts := channels.NewDraftStore()
	tool := NewSlackDraftTool(drafts)
	if tool.RequiresApproval(nil) {
		t.Error("slack_draft should not require approval")
	}
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"channel":"C0123ABC","thread_ts":"1720000000.000100","body":"on it — checking now"}`))
	if err != nil {
		t.Fatal(err)
	}
	id := out.Metadata["draft_id"]
	if id == "" {
		t.Fatal("expected a draft_id")
	}
	d, ok := drafts.Get(id)
	if !ok || d.To != "C0123ABC" || d.Body != "on it — checking now" ||
		d.InReplyTo != "1720000000.000100" {
		t.Errorf("draft not stored correctly (To=channel, InReplyTo=thread_ts): %+v", d)
	}
	if !strings.Contains(out.Content, "on it — checking now") {
		t.Error("preview should include the body")
	}

	// Missing fields -> validation error output.
	bad, _ := tool.Execute(context.Background(), json.RawMessage(`{"channel":"C0123ABC"}`))
	if bad == nil || bad.Error == "" {
		t.Error("expected a validation error for missing body")
	}
}

func TestSlackSendToolRequiresApproval(t *testing.T) {
	tool := NewSlackSendTool(channels.NewSlackAdapter(msg.ChannelConfig{}), channels.NewDraftStore(), nil)
	if !tool.RequiresApproval(nil) {
		t.Error("slack_send MUST require approval")
	}
}

func TestSlackSendToolResolvesDraft(t *testing.T) {
	drafts := channels.NewDraftStore()
	id := drafts.Create("slack", "C0123ABC", "", "hello", "1720000000.000100")
	// Adapter is not connected, so Send fails — but this proves the tool resolves
	// the draft (gets past the lookup) and surfaces the adapter error cleanly.
	tool := NewSlackSendTool(channels.NewSlackAdapter(msg.ChannelConfig{}), drafts, nil)
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"draft_id":"`+id+`"}`))
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if out == nil || out.Error == "" || !strings.Contains(out.Error, "not connected") {
		t.Errorf("expected a 'not connected' error output, got: %+v", out)
	}

	// Unknown draft -> missing error.
	missing, _ := tool.Execute(context.Background(), json.RawMessage(`{"draft_id":"draft-999"}`))
	if missing == nil || missing.Error == "" {
		t.Error("expected error output for unknown draft")
	}
}

func TestSlackListReadEmpty(t *testing.T) {
	ad := channels.NewSlackAdapter(msg.ChannelConfig{})
	list := NewSlackListTool(ad)
	out, _ := list.Execute(context.Background(), json.RawMessage(`{}`))
	if out == nil || !strings.Contains(out.Content, "No Slack messages") {
		t.Errorf("empty list: %+v", out)
	}

	read := NewSlackReadTool(ad)
	missing, _ := read.Execute(context.Background(), json.RawMessage(`{"id":"zzzz"}`))
	if missing == nil || missing.Error == "" {
		t.Error("expected error output for unknown message ID")
	}
}
