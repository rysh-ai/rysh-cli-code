// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"net"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newHumanoidForQueueTest builds a minimal HumanoidActor over a real embedded
// NATS publisher (its prompt Sends go to NATS with no subscriber — harmless).
// activeChatPaneID is left empty so external-buffer echoes are skipped.
func newHumanoidForQueueTest(t *testing.T) *HumanoidActor {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	b, err := bus.New(bus.Config{
		Mode:        "embedded",
		SessionName: "humanoid-queue-test",
		DataDir:     t.TempDir(),
		Port:        port,
	})
	if err != nil {
		t.Skipf("embedded NATS unavailable: %v", err)
	}
	t.Cleanup(b.Close)

	return &HumanoidActor{
		name:               "queue-bot",
		active:             true,
		pub:                b.Publisher(),
		nc:                 b.Conn(),
		llmPromptExecInbox: msg.T("pane", "queue-bot", "llm_prompt_execution", "inbox"),
		conversations:      make(map[string]*ConversationContext),
	}
}

func qInbound(thread, content string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "slack",
		SenderID:    "U1",
		SenderName:  "Tester",
		ThreadID:    thread,
		Content:     content,
	}
}

// TestHumanoidInboundQueueSerializes verifies Problem 3: rapid inbound messages
// are processed strictly one-at-a-time in FIFO order. Previously each inbound
// fired its own prompt, cancelling the in-flight one (last-prompt-wins), which
// caused [rejected] messages and a single reply mixing several threads.
func TestHumanoidInboundQueueSerializes(t *testing.T) {
	h := newHumanoidForQueueTest(t)

	// First message: humanoid idle → dispatched immediately, queue drains.
	h.handleInboundMessage(qInbound("t1", "first"))
	if !h.inboundProcessing {
		t.Fatalf("expected inboundProcessing=true after first message")
	}
	if h.lastInbound == nil || h.lastInbound.ThreadID != "t1" {
		t.Fatalf("lastInbound should be t1, got %+v", h.lastInbound)
	}
	if len(h.inboundQueue) != 0 {
		t.Fatalf("queue should be empty after dispatching first, got %d", len(h.inboundQueue))
	}

	// Two more arrive while busy → queued, NOT processed (lastInbound unchanged).
	h.handleInboundMessage(qInbound("t2", "second"))
	h.handleInboundMessage(qInbound("t3", "third"))
	if len(h.inboundQueue) != 2 {
		t.Fatalf("expected 2 queued while busy, got %d", len(h.inboundQueue))
	}
	if h.lastInbound.ThreadID != "t1" {
		t.Fatalf("lastInbound must stay t1 while busy, got %s", h.lastInbound.ThreadID)
	}

	// Active prompt finishes (done/error) → next message dispatched, FIFO order.
	h.advanceInboundQueue()
	if h.lastInbound.ThreadID != "t2" {
		t.Fatalf("expected t2 after first completes, got %s", h.lastInbound.ThreadID)
	}
	if len(h.inboundQueue) != 1 {
		t.Fatalf("expected 1 queued, got %d", len(h.inboundQueue))
	}

	h.advanceInboundQueue()
	if h.lastInbound.ThreadID != "t3" {
		t.Fatalf("expected t3 next, got %s", h.lastInbound.ThreadID)
	}
	if len(h.inboundQueue) != 0 {
		t.Fatalf("expected empty queue, got %d", len(h.inboundQueue))
	}

	// Final completion → idle, nothing left to process.
	h.advanceInboundQueue()
	if h.inboundProcessing {
		t.Fatalf("expected idle after the queue is drained")
	}
}

// TestHumanoidInboundQueuePausesWhenDeactivated verifies a deactivated humanoid
// does not drain its queue; reactivation resumes it.
func TestHumanoidInboundQueuePausesWhenDeactivated(t *testing.T) {
	h := newHumanoidForQueueTest(t)

	h.handleInboundMessage(qInbound("t1", "first"))  // dispatched
	h.handleInboundMessage(qInbound("t2", "second")) // queued

	// Deactivate, then the active prompt finishes: the queue must NOT advance.
	h.active = false
	h.advanceInboundQueue()
	if h.inboundProcessing {
		t.Fatalf("should not be processing while deactivated")
	}
	if len(h.inboundQueue) != 1 {
		t.Fatalf("queued message should be retained while deactivated, got %d", len(h.inboundQueue))
	}

	// Reactivate → resume processing the queued message.
	h.active = true
	h.dispatchNextInbound()
	if !h.inboundProcessing || h.lastInbound.ThreadID != "t2" {
		t.Fatalf("expected t2 to resume after reactivation, got processing=%v last=%v",
			h.inboundProcessing, h.lastInbound)
	}
}
