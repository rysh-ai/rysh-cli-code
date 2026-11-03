// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 093 H3: `rysh family serve` delivers a family member's text with
// MsgPaneExecPrompt{Literal: true}. Whatever the text starts with — "##",
// "##>", "@@", "#" — it reaches the pane's LLM actor as exactly one
// MsgAgenticPrompt carrying the text verbatim. Without Literal, "##>" keeps
// its old meaning (a pipeline event, no prompt).
func TestPaneExecPrompt_LiteralIsAlwaysAnAgentPrompt(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	llmInbox := msg.T("pane", "lit-pane", "llm_prompt_execution", "inbox")
	p := &PaneActor{id: "lit-pane", pub: pub, nc: nc, llmPromptExecInbox: llmInbox}

	sub, err := nc.SubscribeSync(llmInbox)
	if err != nil {
		t.Fatal(err)
	}
	metaSub, err := nc.SubscribeSync(msg.T("pane", "lit-pane", "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()

	prompts := func() []string {
		t.Helper()
		var out []string
		for {
			m, err := sub.NextMsg(300 * time.Millisecond)
			if err == nats.ErrTimeout {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			var env msg.NATSEnvelope
			if json.Unmarshal(m.Data, &env) != nil || env.TypeTag != msg.TagAgenticPrompt {
				t.Fatalf("unexpected message on the llm inbox: %s", m.Data)
			}
			var ap msg.MsgAgenticPrompt
			if err := json.Unmarshal(env.Payload, &ap); err != nil {
				t.Fatal(err)
			}
			out = append(out, ap.Prompt)
		}
	}

	for _, text := range []string{"##pane profile clear bo", "##> looks like a pipeline event", "@@bo stop", "#cost", "plain"} {
		p.handleExecPrompt(&msg.MsgPaneExecPrompt{Prompt: text, Literal: true})
		_ = nc.Flush()
		if got := prompts(); len(got) != 1 || got[0] != text {
			t.Fatalf("literal %q: llm inbox got %q, want exactly the text", text, got)
		}
	}
	// No pane command was issued as a side effect (nothing sent to the pane's
	// own inbox, where ##pane meta writes would land).
	if m, err := metaSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Fatalf("a literal prompt caused a pane message: %s", m.Data)
	}

	// Unchanged without Literal: "##>" is still a pipeline event.
	p.handleExecPrompt(&msg.MsgPaneExecPrompt{Prompt: "##> event"})
	_ = nc.Flush()
	if got := prompts(); len(got) != 0 {
		t.Fatalf("non-literal ##> reached the LLM: %q", got)
	}
	p.handleExecPrompt(&msg.MsgPaneExecPrompt{Prompt: "##pane x"})
	_ = nc.Flush()
	if got := prompts(); len(got) != 1 || got[0] != "##pane x" {
		t.Fatalf("non-literal ## prompt: %q", got)
	}
}
