// SPDX-License-Identifier: Apache-2.0

package actors

// E33 slice E2 (G7, design 086 §3 option a, root ruling 2026-10-28): the owner
// reaches one pane from the chat with `/panes` and `/use <pane>`. The
// assistant still mediates every turn: a pin only adds an instruction to the
// assistant's prompt, so the message reaches the pane through the assistant's
// own pane_send tool and its approval gate. No new remote-input path.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

var pinTestPanes = []paneRef{
	{ID: "a1b2c3d4e5f6", Name: "builder"},
	{ID: "f6e5d4c3b2a1", Name: "claude-1"},
	{ID: "f6e5aaaa0000", Name: "claude-2"},
}

func newPinAssistant(t *testing.T) (*HumanoidActor, *captureAdapter, *nats.Subscription) {
	t.Helper()
	h, adapter, _ := newAssistantForTest(t, "owner-1")
	h.listPanes = func() ([]paneRef, error) { return pinTestPanes, nil }
	llm, err := h.nc.SubscribeSync(h.llmPromptExecInbox)
	if err != nil {
		t.Fatal(err)
	}
	return h, adapter, llm
}

// lastPrompt returns the prompts sent to the assistant's LLM since the last call.
func promptsSent(t *testing.T, sub *nats.Subscription) []string {
	t.Helper()
	var out []string
	for _, e := range drainSubject(t, sub, 300*time.Millisecond) {
		var p msg.MsgAgenticPrompt
		if json.Unmarshal(e.Payload, &p) == nil && p.Prompt != "" {
			out = append(out, p.Prompt)
		}
	}
	return out
}

// finishTurn lets the next inbound message be processed, as a done run does.
func finishTurn(h *HumanoidActor) {
	h.inboundProcessing = false
	h.inboundQueue = nil
}

func TestIsPaneCommandIsStrict(t *testing.T) {
	for _, s := range []string{"/panes", " /PANES ", "/use builder", "/use off", "/use"} {
		if !isPaneCommand(s) {
			t.Errorf("%q should be a pane command", s)
		}
	}
	for _, s := range []string{"use the builder pane", "panes?", "/usefoo", "/panesx", "please /use builder", ""} {
		if isPaneCommand(s) {
			t.Errorf("%q must be a prompt, not a pane command", s)
		}
	}
}

func TestPanesListsNamesOverTheChat(t *testing.T) {
	h, adapter, llm := newPinAssistant(t)
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "/panes"))
	if len(adapter.sent) != 1 {
		t.Fatalf("want one reply, got %d", len(adapter.sent))
	}
	got := adapter.sent[0]
	if got.RecipientID != "owner-1" || got.ThreadID != "chat-1" {
		t.Fatalf("reply routed to %+v", got)
	}
	for _, p := range pinTestPanes {
		if !strings.Contains(got.Content, p.Name) {
			t.Errorf("list %q is missing %s", got.Content, p.Name)
		}
	}
	if p := promptsSent(t, llm); len(p) != 0 {
		t.Fatalf("/panes reached the LLM: %v", p)
	}
}

func TestUsePinsTheThreadThroughTheAssistant(t *testing.T) {
	h, adapter, llm := newPinAssistant(t)

	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "/use builder"))
	if len(adapter.sent) != 1 || !strings.Contains(adapter.sent[0].Content, "builder") {
		t.Fatalf("pin not confirmed: %+v", adapter.sent)
	}
	if p := promptsSent(t, llm); len(p) != 0 {
		t.Fatalf("/use reached the LLM: %v", p)
	}

	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "run the tests"))
	p := promptsSent(t, llm)
	if len(p) != 1 {
		t.Fatalf("want one prompt, got %d", len(p))
	}
	for _, want := range []string{"a1b2c3d4e5f6", "pane_send", "run the tests"} {
		if !strings.Contains(p[0], want) {
			t.Errorf("pinned prompt lacks %q:\n%s", want, p[0])
		}
	}

	// Another thread is not pinned.
	finishTurn(h)
	h.handleInboundMessage(remoteDM("owner-1", "chat-2", "hello"))
	if p := promptsSent(t, llm); len(p) != 1 || strings.Contains(p[0], "a1b2c3d4e5f6") {
		t.Fatalf("the pin leaked to another thread: %v", p)
	}

	// /use off ends it.
	finishTurn(h)
	adapter.sent = nil
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "/use off"))
	if len(adapter.sent) != 1 {
		t.Fatalf("unpin not confirmed: %+v", adapter.sent)
	}
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "hello again"))
	if p := promptsSent(t, llm); len(p) != 1 || strings.Contains(p[0], "a1b2c3d4e5f6") {
		t.Fatalf("still pinned after /use off: %v", p)
	}
}

func TestUseByIDPrefixAndItsRefusals(t *testing.T) {
	cases := []struct {
		arg, want string
		pinned    string
	}{
		{arg: "a1b2", want: "builder", pinned: "a1b2c3d4e5f6"},
		{arg: "f6e5", want: "more than one", pinned: ""},
		{arg: "nope", want: "no pane", pinned: ""},
		{arg: "a1", want: "no pane", pinned: ""}, // an id prefix needs 4 characters
	}
	for _, c := range cases {
		t.Run(c.arg, func(t *testing.T) {
			h, adapter, _ := newPinAssistant(t)
			h.handleInboundMessage(remoteDM("owner-1", "chat-1", "/use "+c.arg))
			if len(adapter.sent) != 1 || !strings.Contains(adapter.sent[0].Content, c.want) {
				t.Fatalf("reply %+v, want it to contain %q", adapter.sent, c.want)
			}
			if got := h.panePins["telegram/chat-1"].ID; got != c.pinned {
				t.Fatalf("pinned %q, want %q", got, c.pinned)
			}
		})
	}
}

func TestPaneCommandsFromAStrangerAreHeld(t *testing.T) {
	h, adapter, _ := newPinAssistant(t)
	called := false
	h.listPanes = func() ([]paneRef, error) { called = true; return pinTestPanes, nil }
	h.handleInboundMessage(remoteDM("stranger", "chat-9", "/panes"))
	h.handleInboundMessage(remoteDM("stranger", "chat-9", "/use builder"))
	if called || len(h.panePins) != 0 {
		t.Fatal("a stranger's pane command was acted on")
	}
	for _, s := range adapter.sent {
		if strings.Contains(s.Content, "builder") {
			t.Fatalf("a stranger was shown the panes: %q", s.Content)
		}
	}
}

// Pane commands answer only in the owner's 1:1 DM: in a group, other members
// would see the session's pane names, and a pin would let the group's
// messages reach a pane.
func TestPaneCommandsOnlyInADirectMessage(t *testing.T) {
	h, adapter, llm := newPinAssistant(t)
	called := false
	h.listPanes = func() ([]paneRef, error) { called = true; return pinTestPanes, nil }
	for _, c := range []string{"/panes", "/use builder"} {
		m := remoteDM("owner-1", "group-1", c)
		m.Metadata = map[string]string{"chat_type": "group", "mention": "true"}
		h.handleInboundMessage(m)
	}
	if called || len(h.panePins) != 0 {
		t.Fatal("a pane command was acted on in a group")
	}
	if len(adapter.sent) != 2 {
		t.Fatalf("want a DM-only notice per command, got %+v", adapter.sent)
	}
	for _, s := range adapter.sent {
		if !strings.Contains(s.Content, "direct message") || strings.Contains(s.Content, "builder") {
			t.Fatalf("group reply %q: want a DM-only notice and no pane names", s.Content)
		}
	}
	if p := promptsSent(t, llm); len(p) != 0 {
		t.Fatalf("a group pane command reached the LLM: %v", p)
	}
}

// On a channel with no owner allowlist the pairing gate admits anyone, so a
// pane command there is refused too: only an owner on a gated channel.
func TestPaneCommandsRefusedOnAnUngatedChannel(t *testing.T) {
	h, adapter, _ := newPinAssistant(t)
	h.contacts = map[string]msg.ChannelConfig{"telegram": {Enabled: true, ReplyMode: "messages"}}
	called := false
	h.listPanes = func() ([]paneRef, error) { called = true; return pinTestPanes, nil }
	h.handleInboundMessage(remoteDM("anyone", "chat-7", "/panes"))
	h.handleInboundMessage(remoteDM("anyone", "chat-7", "/use builder"))
	if called || len(h.panePins) != 0 {
		t.Fatal("a pane command was acted on over an ungated channel")
	}
	for _, s := range adapter.sent {
		if strings.Contains(s.Content, "builder") {
			t.Fatalf("pane names shown over an ungated channel: %q", s.Content)
		}
	}
}
