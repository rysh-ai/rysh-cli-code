// SPDX-License-Identifier: Apache-2.0

package actors

// N1 (security-lead, sprint 0.2.16): the turn pin that holds a pinned chat
// thread's pane_send to its pane ends with that turn, so a later prompt typed
// in the pane is not held to the last pinned pane. It must never end early:
// the executor runs the newest prompt it is sent (an older run is cancelled
// and ends "paused"), sub-agent runs report on the same status subject, and a
// paused run can be continued, so until the newest prompt's own run is done
// pane_send stays held to the pinned pane.

import (
	"testing"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// msgOnlyCtx delivers one message to Receive; the branches under test read
// nothing else from the context.
type msgOnlyCtx struct {
	actor.Context
	m interface{}
}

func (c msgOnlyCtx) Message() interface{} { return c.m }

func deliver(h *HumanoidActor, m interface{}) { h.Receive(msgOnlyCtx{m: m}) }

func runStatus(h *HumanoidActor, orch, phase string) {
	deliver(h, &msg.MsgAgenticStatus{OrchestratorID: orch, Phase: phase})
}

// pinnedTurn pins chat-1 to the builder pane and sends one message from it.
func pinnedTurn(t *testing.T) (*HumanoidActor, string) {
	t.Helper()
	h, _, llm := newPinAssistant(t)
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "/use builder"))
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "run the tests"))
	if p := promptsSent(t, llm); len(p) != 1 {
		t.Fatalf("want the pinned prompt sent, got %d", len(p))
	}
	pin := pinTestPanes[0].ID
	if got := h.currentTurnPin(); got != pin {
		t.Fatalf("turn pin = %q, want %q", got, pin)
	}
	return h, pin
}

func wantPin(t *testing.T, h *HumanoidActor, want, when string) {
	t.Helper()
	if got := h.currentTurnPin(); got != want {
		t.Fatalf("%s: turn pin = %q, want %q", when, got, want)
	}
}

func TestTurnPinEndsWithThePinnedTurn(t *testing.T) {
	h, pin := pinnedTurn(t)
	runStatus(h, "run-c", "planning")
	wantPin(t, h, pin, "while the pinned run works")
	runStatus(h, "run-c", "done")
	wantPin(t, h, "", "after the pinned run is done")

	deliver(h, &msg.MsgAgenticPrompt{Prompt: "check the build"})
	wantPin(t, h, "", "a prompt typed in the pane after the pinned turn")
}

func TestTurnPinEndsOnError(t *testing.T) {
	h, _ := pinnedTurn(t)
	runStatus(h, "run-c", "planning")
	runStatus(h, "run-c", "error")
	wantPin(t, h, "", "after the pinned run failed")
}

// The pane-typed run was in flight when the pinned prompt was sent: its end
// (cancelled by the pinned prompt) is not the pinned turn's end.
func TestTurnPinOutlivesTheRunItCancelled(t *testing.T) {
	h, _, _ := newPinAssistant(t)
	deliver(h, &msg.MsgAgenticPrompt{Prompt: "check the build"})
	runStatus(h, "run-o", "planning")
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "/use builder"))
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "run the tests"))
	pin := pinTestPanes[0].ID
	runStatus(h, "run-o", "paused")
	wantPin(t, h, pin, "the cancelled pane-typed run ended")
	runStatus(h, "run-c", "planning")
	wantPin(t, h, pin, "the pinned run started")
	runStatus(h, "run-c", "done")
	wantPin(t, h, "", "the pinned run is done")
}

// Same, but the pinned prompt is sent before the humanoid has seen the
// pane-typed run start.
func TestTurnPinOutlivesARunNotYetSeen(t *testing.T) {
	h, _, _ := newPinAssistant(t)
	deliver(h, &msg.MsgAgenticPrompt{Prompt: "check the build"})
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "/use builder"))
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "run the tests"))
	pin := pinTestPanes[0].ID
	runStatus(h, "run-o", "planning")
	runStatus(h, "run-o", "done") // finished before the pinned prompt reached it
	wantPin(t, h, pin, "the older run ended")
	runStatus(h, "run-c", "planning")
	runStatus(h, "run-c", "done")
	wantPin(t, h, "", "the pinned run is done")
}

func TestTurnPinIgnoresSubAgentRuns(t *testing.T) {
	h, pin := pinnedTurn(t)
	runStatus(h, "run-c", "planning")
	runStatus(h, "sub-1", "planning")
	runStatus(h, "sub-1", "done")
	wantPin(t, h, pin, "a sub-agent of the pinned run finished")
	runStatus(h, "sub-2", "error")
	wantPin(t, h, pin, "a sub-agent of the pinned run failed at once")
	runStatus(h, "run-c", "done")
	wantPin(t, h, "", "the pinned run is done")
}

// A paused pinned run is resumable: its continuation is still the pinned turn.
func TestTurnPinHoldsAcrossPauseAndContinue(t *testing.T) {
	h, pin := pinnedTurn(t)
	runStatus(h, "run-c", "planning")
	runStatus(h, "run-c", "paused")
	wantPin(t, h, pin, "the pinned run paused")
	deliver(h, &msg.MsgHumanoidContinue{})
	runStatus(h, "run-c2", "planning")
	wantPin(t, h, pin, "the continuation runs")
	runStatus(h, "run-c2", "done")
	wantPin(t, h, "", "the continuation is done")
}

// A prompt typed in the pane while the pinned run works cancels it; the pin
// holds until the typed prompt's own run ends, never in between.
func TestTurnPinHoldsUntilTheNewestRunEnds(t *testing.T) {
	h, pin := pinnedTurn(t)
	runStatus(h, "run-c", "planning")
	deliver(h, &msg.MsgAgenticPrompt{Prompt: "check the build"})
	runStatus(h, "run-c", "paused")
	wantPin(t, h, pin, "the pinned run was cancelled by the typed prompt")
	runStatus(h, "run-o", "planning")
	wantPin(t, h, pin, "the typed prompt's run started")
	runStatus(h, "run-o", "done")
	wantPin(t, h, "", "the newest run is done")
}

// Another thread's message no longer drops the pin before its own run starts.
func TestUnpinnedThreadDoesNotDropThePinEarly(t *testing.T) {
	h, pin := pinnedTurn(t)
	runStatus(h, "run-c", "planning")
	runStatus(h, "run-c", "paused") // iteration limit: the inbound queue stays
	finishTurn(h)
	h.handleInboundMessage(remoteDM("owner-1", "chat-2", "hello"))
	wantPin(t, h, pin, "another thread's prompt was sent")
	runStatus(h, "run-d", "planning")
	runStatus(h, "run-d", "done")
	wantPin(t, h, "", "that thread's run is done")
}
