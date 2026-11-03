// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func captureLifecycle(t *testing.T) *[]paneEvent {
	t.Helper()
	var got []paneEvent
	prev := dispatchPaneEvent
	dispatchPaneEvent = func(ev paneEvent, dir func() string) {
		ev.dir = dir()
		got = append(got, ev)
	}
	t.Cleanup(func() { dispatchPaneEvent = prev })
	return &got
}

func TestPaneLifecycle_NeedsYouRisingEdgeOnly(t *testing.T) {
	got := captureLifecycle(t)
	p := &PaneActor{id: "p1"}
	p.shellCwdAtomic.Store("/work")
	p.applyPromptAttention(true)
	p.applyPromptAttention(false)
	if len(*got) != 1 || (*got)[0].kind != paneEventNeedsYou || (*got)[0].reason != "prompt" ||
		(*got)[0].pane != "p1" || (*got)[0].dir != "/work" {
		t.Fatalf("events: %+v", *got)
	}
	// A native approval while nothing was pending is a rising edge too.
	p.noteApprovalRequest(&msg.MsgApprovalRequest{RequestID: "r1"})
	if len(*got) != 2 || (*got)[1].reason != "approval" {
		t.Fatalf("events: %+v", *got)
	}
}

func TestPaneLifecycle_ProgramStartAndExit(t *testing.T) {
	got := captureLifecycle(t)
	p := &PaneActor{id: "p1"}
	p.noteProgramEvents(foregroundEvents("", "claude"))
	p.noteProgramEvents(foregroundEvents("claude", ""))
	if len(*got) != 2 || (*got)[0].kind != paneEventAgentStart || (*got)[1].kind != paneEventFinish ||
		(*got)[0].program != "claude" || (*got)[1].program != "claude" {
		t.Fatalf("events: %+v", *got)
	}
}

// The pane tells the lifecycle whether its agent is sandboxed, from the
// agent.sandbox stamp, lock-free (the edges come from the read loop and the
// foreground poller, not only the mailbox).
func TestPaneLifecycle_EventsCarryTheSandboxStamp(t *testing.T) {
	got := captureLifecycle(t)
	p := &PaneActor{id: "p1"}
	p.setMeta(metaAgentSandbox, "strict")
	p.noteProgramEvents(foregroundEvents("", "claude"))
	p.setMeta(metaAgentSandbox, "")
	p.noteProgramEvents(foregroundEvents("claude", ""))
	applyBirthMeta(p, map[string]string{metaAgentSandbox: "strict"})
	p.noteAttentionEdge("", "prompt")
	if len(*got) != 3 || !(*got)[0].sandboxed || (*got)[1].sandboxed || !(*got)[2].sandboxed {
		t.Fatalf("events: %+v", *got)
	}
}
