// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestEvictConversationMessages_NoEviction(t *testing.T) {
	buf := make([]*msg.ConversationMessage, 5)
	for i := range buf {
		buf[i] = &msg.ConversationMessage{TurnID: "t", Content: "x"}
	}
	evicted := evictConversationMessages(&buf, 10)
	if evicted != nil {
		t.Errorf("expected nil eviction, got %d messages", len(evicted))
	}
	if len(buf) != 5 {
		t.Errorf("expected 5 messages, got %d", len(buf))
	}
}

func TestEvictConversationMessages_WithEviction(t *testing.T) {
	buf := make([]*msg.ConversationMessage, 10)
	for i := range buf {
		buf[i] = &msg.ConversationMessage{TurnID: "t", Content: string(rune('A' + i))}
	}
	evicted := evictConversationMessages(&buf, 7)
	if len(evicted) != 3 {
		t.Fatalf("expected 3 evicted, got %d", len(evicted))
	}
	if len(buf) != 7 {
		t.Fatalf("expected 7 remaining, got %d", len(buf))
	}
	// First evicted should be 'A'
	if evicted[0].Content != "A" {
		t.Errorf("evicted[0].Content: got %q, want 'A'", evicted[0].Content)
	}
	// First remaining should be 'D'
	if buf[0].Content != "D" {
		t.Errorf("buf[0].Content: got %q, want 'D'", buf[0].Content)
	}
}

func TestEvictConversationMessages_ZeroLimit(t *testing.T) {
	buf := make([]*msg.ConversationMessage, 3)
	for i := range buf {
		buf[i] = &msg.ConversationMessage{Content: "x"}
	}
	evicted := evictConversationMessages(&buf, 0)
	if evicted != nil {
		t.Errorf("expected nil eviction for limit=0, got %d", len(evicted))
	}
}

func TestMergeOldestEntries(t *testing.T) {
	m := &MemoryManagerActor{
		paneID: "test-pane",
		states: make(map[msg.ConversationType]*msg.MemoryState),
	}

	state := &msg.MemoryState{
		PaneID: "test-pane",
		Mode:   msg.ConvAI,
		Entries: []msg.MemoryEntry{
			{ID: "e1", Summary: "First batch", TurnIDs: []string{"t1", "t2"}, TurnCount: 2},
			{ID: "e2", Summary: "Second batch", TurnIDs: []string{"t3", "t4"}, TurnCount: 2},
			{ID: "e3", Summary: "Third batch", TurnIDs: []string{"t5"}, TurnCount: 1},
		},
	}

	m.mergeOldestEntries(state)

	if len(state.Entries) != 2 {
		t.Fatalf("expected 2 entries after merge, got %d", len(state.Entries))
	}

	// First entry should be the merged one.
	merged := state.Entries[0]
	if merged.ID != "e1" {
		t.Errorf("merged ID: got %q, want 'e1'", merged.ID)
	}
	if merged.TurnCount != 4 {
		t.Errorf("merged TurnCount: got %d, want 4", merged.TurnCount)
	}
	if len(merged.TurnIDs) != 4 {
		t.Errorf("merged TurnIDs: got %d, want 4", len(merged.TurnIDs))
	}

	// Second entry should be unchanged.
	if state.Entries[1].ID != "e3" {
		t.Errorf("second entry ID: got %q, want 'e3'", state.Entries[1].ID)
	}
}

func TestTriggerMemorySummarization_OnlyForMemoryCapable(t *testing.T) {
	// Verify HasMemory() is correctly used.
	tests := []struct {
		ct     msg.ConversationType
		hasMem bool
	}{
		{msg.ConvShell, false},
		{msg.ConvAI, true},
		{msg.ConvRysh, false},
		{msg.ConvChat, false},
		{msg.ConvEmail, true},
		{msg.ConvSlack, true},
		{msg.ConvChatbot, true},
	}
	for _, tt := range tests {
		if got := tt.ct.HasMemory(); got != tt.hasMem {
			t.Errorf("HasMemory(%q): got %v, want %v", tt.ct, got, tt.hasMem)
		}
	}
}
