// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// QA-33 (found by video 068): a ## command typed in prompt or chat mode ran,
// but its echo and result went only to the shell and rysh buffers, so nothing
// appeared in the view the user typed it in; and ##history listed the shell
// history there instead of the mode's own.

type paneCapture struct {
	subject, kind, content string
}

// collectPaneOutput drains sub, keeping conversation appends and the
// display-only per-mode appends with their kind.
func collectPaneOutput(t *testing.T, sub *nats.Subscription, codecs *msg.CodecRegistry) []paneCapture {
	t.Helper()
	var caps []paneCapture
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m, err := sub.NextMsg(150 * time.Millisecond)
		if err != nil {
			break
		}
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			continue
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			continue
		}
		switch dm := decoded.(type) {
		case *msg.MsgConversationAppend:
			if dm.Message != nil {
				caps = append(caps, paneCapture{m.Subject, "conversation", dm.Message.Content})
			}
		case *msg.MsgPaneAIOutputAppend:
			caps = append(caps, paneCapture{m.Subject, "display", dm.Text})
		case *msg.MsgPaneChatOutputAppend:
			caps = append(caps, paneCapture{m.Subject, "display", dm.Text})
		}
	}
	return caps
}

func submitInMode(t *testing.T, w *WorkspaceActor, nc *nats.Conn, codecs *msg.CodecRegistry, paneID, mode, text string) []paneCapture {
	t.Helper()
	sub, err := nc.SubscribeSync(msg.T("pane", paneID, ">"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	w.handleSubmitInput(nil, &msg.MsgSubmitInput{Text: text, Mode: mode})
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return collectPaneOutput(t, sub, codecs)
}

func modeViewWorkspace(t *testing.T, paneID string) (*WorkspaceActor, *nats.Conn, *msg.CodecRegistry) {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	w := &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		activePaneID: paneID,
		tabs:         []*tabInfo{{id: "tab-1", title: "tab", actor: &TabActor{}}},
		activeTabIdx: 0,
	}
	return w, nc, codecs
}

func TestHashCommandShowsInTheViewItWasTypedIn(t *testing.T) {
	const paneID = "test-pane"
	for _, tc := range []struct{ mode, subject string }{
		{"prompt", msg.T("pane", paneID, "output", "ai")},
		{"chat", msg.T("pane", paneID, "output", "chat")},
	} {
		w, nc, codecs := modeViewWorkspace(t, paneID)
		caps := submitInMode(t, w, nc, codecs, paneID, tc.mode, "##help")
		var echo, result bool
		for _, c := range caps {
			if c.subject != tc.subject {
				continue
			}
			if c.kind != "display" {
				// A conversation append on the AI buffer would feed the
				// pane's memory summaries, i.e. the LLM's context.
				t.Errorf("%s mode: ## output reached %s as a %s message, want display-only", tc.mode, tc.subject, c.kind)
			}
			echo = echo || strings.Contains(c.content, "##help")
			result = result || strings.Contains(c.content, "available ## commands:")
		}
		if !echo || !result {
			t.Errorf("%s mode: echo shown %v, result shown %v in %s; captured=%+v", tc.mode, echo, result, tc.subject, caps)
		}
	}
}

// Shell mode is unchanged: nothing goes to the prompt or chat view.
func TestHashCommandInShellModeStaysOutOfOtherViews(t *testing.T) {
	const paneID = "test-pane"
	w, nc, codecs := modeViewWorkspace(t, paneID)
	for _, c := range submitInMode(t, w, nc, codecs, paneID, "shell", "##help") {
		if c.subject == msg.T("pane", paneID, "output", "ai") || c.subject == msg.T("pane", paneID, "output", "chat") {
			t.Errorf("shell-mode ##help reached %s: %+v", c.subject, c)
		}
	}
}

// ##history lists the history of the mode it was typed in.
func TestHashHistoryFollowsTheTypedMode(t *testing.T) {
	const paneID = "test-pane"
	for _, tc := range []struct{ mode, want string }{
		{"prompt", "AI prompts history"},
		{"shell", "shell commands history"},
	} {
		w, nc, codecs := modeViewWorkspace(t, paneID)
		found := false
		for _, c := range submitInMode(t, w, nc, codecs, paneID, tc.mode, "##history") {
			found = found || strings.Contains(c.content, tc.want)
		}
		if !found {
			t.Errorf("##history typed in %s mode did not list %q", tc.mode, tc.want)
		}
	}
}

// listenerMsgCtx delivers one message to a listener's Receive; the branch
// under test reads nothing else from the context.
type listenerMsgCtx struct {
	actor.Context
	m interface{}
}

func (c listenerMsgCtx) Message() interface{} { return c.m }

// A pane listening to another (##pane listen) passes the display-only prompt
// view text on as display-only: never as an AI conversation turn in the
// listening pane, which would feed its memory summaries.
func TestListenerForwardsPromptViewTextAsDisplayOnly(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	const owner = "owner-pane"
	l := &PaneSharedOutputListenerActor{ownerPaneID: owner, targetPaneID: "target-pane", targetAlias: "t", pub: msg.NewNATSPublisher(nc, codecs), nc: nc}
	sub, err := nc.SubscribeSync(msg.T("pane", owner, ">"))
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	l.Receive(listenerMsgCtx{m: &msg.MsgPaneAIOutputAppend{Text: "##help output"}})
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	shown := false
	for _, c := range collectPaneOutput(t, sub, codecs) {
		if c.kind == "conversation" && strings.Contains(c.content, "##help output") {
			t.Errorf("forwarded as a conversation turn on %s", c.subject)
		}
		shown = shown || (c.kind == "display" && c.subject == msg.T("pane", owner, "output", "ai"))
	}
	if !shown {
		t.Error("the text did not reach the listening pane's prompt view")
	}
}
