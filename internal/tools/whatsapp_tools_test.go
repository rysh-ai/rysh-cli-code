package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestWhatsAppDraftTool(t *testing.T) {
	drafts := channels.NewDraftStore()
	tool := NewWhatsAppDraftTool(drafts)
	if tool.RequiresApproval(nil) {
		t.Error("whatsapp_draft should not require approval")
	}
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"to":"447700900123","body":"on my way"}`))
	if err != nil {
		t.Fatal(err)
	}
	id := out.Metadata["draft_id"]
	if id == "" {
		t.Fatal("expected a draft_id")
	}
	d, ok := drafts.Get(id)
	if !ok || d.To != "447700900123" || d.Body != "on my way" {
		t.Errorf("draft not stored correctly: %+v", d)
	}
	if !strings.Contains(out.Content, "on my way") {
		t.Error("preview should include the body")
	}

	// Missing fields -> validation error output.
	bad, _ := tool.Execute(context.Background(), json.RawMessage(`{"to":"x"}`))
	if bad == nil || bad.Error == "" {
		t.Error("expected a validation error for missing body")
	}
}

func TestWhatsAppSendToolRequiresApproval(t *testing.T) {
	tool := NewWhatsAppSendTool(channels.NewWhatsAppAdapter(msg.ChannelConfig{}), channels.NewDraftStore())
	if !tool.RequiresApproval(nil) {
		t.Error("whatsapp_send MUST require approval")
	}
}

func TestWhatsAppSendToolResolvesDraft(t *testing.T) {
	drafts := channels.NewDraftStore()
	id := drafts.Create("447700900123", "", "hello", "")
	// Adapter is not connected, so Send fails — but this proves the tool resolves
	// the draft (gets past the lookup) and surfaces the adapter error cleanly.
	tool := NewWhatsAppSendTool(channels.NewWhatsAppAdapter(msg.ChannelConfig{}), drafts)
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

func TestWhatsAppSendTemplateToolRequiresApproval(t *testing.T) {
	tool := NewWhatsAppSendTemplateTool(channels.NewWhatsAppAdapter(msg.ChannelConfig{}))
	if !tool.RequiresApproval(nil) {
		t.Error("whatsapp_send_template MUST require approval")
	}
	if !tool.Spec().RequiresApproval {
		t.Error("whatsapp_send_template spec MUST declare RequiresApproval")
	}
}

func TestWhatsAppSendTemplateToolValidation(t *testing.T) {
	tool := NewWhatsAppSendTemplateTool(channels.NewWhatsAppAdapter(msg.ChannelConfig{}))

	// Missing template_name -> validation error output (not a hard error).
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"to":"447700900123"}`))
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if out == nil || out.Error == "" || !strings.Contains(out.Error, "template_name") {
		t.Errorf("expected a validation error naming template_name, got: %+v", out)
	}

	// Missing to -> validation error output.
	out, _ = tool.Execute(context.Background(), json.RawMessage(`{"template_name":"hello_world"}`))
	if out == nil || out.Error == "" {
		t.Errorf("expected a validation error for missing to, got: %+v", out)
	}

	// Malformed JSON -> hard error (mirrors the other whatsapp_* tools).
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{`)); err == nil {
		t.Error("malformed params should be a hard error")
	}
}

func TestWhatsAppSendTemplateToolSurfacesAdapterError(t *testing.T) {
	// Adapter is not connected, so the send fails — this proves the tool passes
	// validation and surfaces the adapter error cleanly (mirrors whatsapp_send).
	tool := NewWhatsAppSendTemplateTool(channels.NewWhatsAppAdapter(msg.ChannelConfig{}))
	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"to":"447700900123","template_name":"hello_world","language":"en_US","params":["Alice"]}`))
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if out == nil || out.Error == "" || !strings.Contains(out.Error, "not connected") {
		t.Errorf("expected a 'not connected' error output, got: %+v", out)
	}
}

func TestWhatsAppListReadEmpty(t *testing.T) {
	ad := channels.NewWhatsAppAdapter(msg.ChannelConfig{})
	list := NewWhatsAppListTool(ad)
	out, _ := list.Execute(context.Background(), json.RawMessage(`{}`))
	if out == nil || !strings.Contains(out.Content, "No WhatsApp messages") {
		t.Errorf("empty list: %+v", out)
	}

	read := NewWhatsAppReadTool(ad)
	r, _ := read.Execute(context.Background(), json.RawMessage(`{"id":"wa-1"}`))
	if r == nil || r.Error == "" {
		t.Errorf("read of missing message should error: %+v", r)
	}
}
