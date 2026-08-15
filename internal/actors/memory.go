// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// ---------------------------------------------------------------------------
// MemoryManagerActor — manages per-pane memory summarization and persistence.
//
// Architecture:
// - One MemoryManagerActor per pane (spawned as child of PaneActor).
// - Subscribes to rysh.pane.{paneID}.memory.summarize for summarization requests.
// - Calls the LLM asynchronously to summarize turns.
// - Stores memory entries in JetStream KV (rysh-memory bucket).
// - Publishes MsgMemoryStateUpdate to the pane's LLMPromptExecutionActor.
// - Publishes MsgMemoryAppend to rysh.pane.{paneID}.memory.{mode} for sharing.
// ---------------------------------------------------------------------------

const (
	memoryKVBucket = "rysh-memory"
	maxMemEntries  = 50 // max entries per pane per mode before merging
)

// MemoryManagerActor manages the memory lifecycle for a single pane.
type MemoryManagerActor struct {
	paneID string
	pub    *msg.NATSPublisher
	nc     *nats.Conn
	br     *bridge.NATSBridge
	prov   provider.AgenticProvider
	js     nats.JetStreamContext

	// In-memory cache of memory states, keyed by ConversationType.
	states map[msg.ConversationType]*msg.MemoryState

	// Subject for summarization requests.
	summarizeSubject string

	// Subject prefix for publishing memory state updates to LLMPromptExecutionActor.
	llmInboxSubject string
}

// NewMemoryManagerActor creates a MemoryManagerActor for the given pane.
func NewMemoryManagerActor(
	paneID string,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	prov provider.AgenticProvider,
) *MemoryManagerActor {
	js, _ := nc.JetStream()
	return &MemoryManagerActor{
		paneID:           paneID,
		pub:              pub,
		nc:               nc,
		prov:             prov,
		js:               js,
		states:           make(map[msg.ConversationType]*msg.MemoryState),
		summarizeSubject: msg.T("pane", paneID, "memory", "summarize"),
		llmInboxSubject:  msg.T("pane", paneID, "llm_prompt_execution", "inbox"),
	}
}

// Receive handles actor lifecycle and NATS messages.
func (m *MemoryManagerActor) Receive(ctx actor.Context) {
	switch mm := ctx.Message().(type) {
	case *actor.Started:
		m.br = bridge.New(m.nc, ctx.Self(), ctx.ActorSystem(), m.pub.Codecs())
		m.br.SetPaneID(m.paneID)
		if err := m.br.AddSubject(m.summarizeSubject); err != nil {
			slog.Error("memory: subscribe summarize", "err", err, "pane", m.paneID)
		}
		// Restore memory state from KV on startup.
		m.restoreFromKV()
		// Push initial memory state to LLMPromptExecutionActor (if any exists).
		m.pushAllMemoryStates()
		slog.Info("memory: started", "pane", m.paneID)

	case *actor.Stopping:
		if m.br != nil {
			m.br.Stop()
		}

	case *msg.MsgMemorySummarize:
		m.handleSummarize(ctx, mm)

	case *msg.MsgMemorySummarizeDone:
		m.handleSummarizeDone(mm)
	}
}

// handleSummarize processes a summarization request asynchronously.
// It runs the LLM call in a goroutine to avoid blocking the actor mailbox.
func (m *MemoryManagerActor) handleSummarize(ctx actor.Context, req *msg.MsgMemorySummarize) {
	if len(req.Turns) == 0 {
		return
	}

	// Capture immutable values for the goroutine.
	paneID := m.paneID
	mode := req.Mode
	turns := req.Turns
	pub := m.pub
	prov := m.prov
	selfPID := ctx.Self()
	system := ctx.ActorSystem()

	go func() {
		// Build summarization prompt.
		prompt := msg.MemorySummarizationPrompt(mode, turns)

		// Call LLM for summarization (use a 30s timeout).
		summarizeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		summary, err := prov.Complete(summarizeCtx, prompt)
		if err != nil {
			slog.Error("memory: summarization failed", "err", err, "pane", paneID, "mode", mode)
			// Publish failure notification.
			_ = pub.Send(msg.T("pane", paneID, "memory", string(mode)), &msg.MsgMemorySummarizeDone{
				PaneID: paneID,
				Mode:   mode,
				Error:  err.Error(),
			})
			return
		}

		// Collect turn IDs.
		turnIDs := make([]string, 0, len(turns))
		seen := make(map[string]bool)
		for _, t := range turns {
			if t.TurnID != "" && !seen[t.TurnID] {
				turnIDs = append(turnIDs, t.TurnID)
				seen[t.TurnID] = true
			}
		}

		// Create the memory entry.
		entry := msg.NewMemoryEntry(mode, summary, turnIDs)

		// Send the result back to the actor's mailbox for sequential processing.
		done := &msg.MsgMemorySummarizeDone{
			PaneID: paneID,
			Mode:   mode,
			Entry:  *entry,
		}
		system.Root.Send(selfPID, done)
	}()
}

// handleSummarizeDone processes the result of an async summarization.
func (m *MemoryManagerActor) handleSummarizeDone(done *msg.MsgMemorySummarizeDone) {
	if done.Error != "" {
		slog.Warn("memory: summarization reported error", "err", done.Error, "pane", m.paneID)
		return
	}

	mode := done.Mode

	// Get or create state for this mode.
	state, ok := m.states[mode]
	if !ok {
		state = &msg.MemoryState{
			PaneID: m.paneID,
			Mode:   mode,
		}
		m.states[mode] = state
	}

	// Append the new entry.
	state.Entries = append(state.Entries, done.Entry)
	state.TotalSummarizedTurns += done.Entry.TurnCount

	// If we exceed max entries, merge the oldest two.
	if len(state.Entries) > maxMemEntries {
		m.mergeOldestEntries(state)
	}

	// Persist to KV.
	m.persistToKV(mode, state)

	// Publish memory append for sharing.
	_ = m.pub.SendMemoryAppend(m.paneID, &done.Entry)

	// Push updated memory state to LLMPromptExecutionActor.
	m.pushMemoryState(mode)
}

// mergeOldestEntries combines the oldest two entries into one.
// This keeps memory bounded while preserving important context.
func (m *MemoryManagerActor) mergeOldestEntries(state *msg.MemoryState) {
	if len(state.Entries) < 2 {
		return
	}

	// Merge first two entries.
	e1 := state.Entries[0]
	e2 := state.Entries[1]

	merged := msg.MemoryEntry{
		ID:          e1.ID, // keep the oldest ID
		Summary:     e1.Summary + "\n\n---\n\n" + e2.Summary,
		TurnIDs:     append(e1.TurnIDs, e2.TurnIDs...),
		Mode:        state.Mode,
		CreatedAtMs: e1.CreatedAtMs,
		TurnCount:   e1.TurnCount + e2.TurnCount,
	}

	// Truncate merged summary if too long (max 2000 chars).
	if len(merged.Summary) > 2000 {
		merged.Summary = merged.Summary[:1997] + "..."
	}

	// Replace the first two entries with the merged one.
	state.Entries = append([]msg.MemoryEntry{merged}, state.Entries[2:]...)
}

// pushMemoryState sends the current memory state to the LLMPromptExecutionActor.
func (m *MemoryManagerActor) pushMemoryState(mode msg.ConversationType) {
	state, ok := m.states[mode]
	if !ok {
		return
	}
	_ = m.pub.Send(m.llmInboxSubject, &msg.MsgMemoryStateUpdate{State: state})
}

// pushAllMemoryStates sends all non-empty memory states to the LLMPromptExecutionActor.
func (m *MemoryManagerActor) pushAllMemoryStates() {
	for _, state := range m.states {
		if len(state.Entries) > 0 {
			_ = m.pub.Send(m.llmInboxSubject, &msg.MsgMemoryStateUpdate{State: state})
		}
	}
}

// ---------------------------------------------------------------------------
// JetStream KV persistence
// ---------------------------------------------------------------------------

func (m *MemoryManagerActor) kvKey(mode msg.ConversationType) string {
	return fmt.Sprintf("%s/%s", m.paneID, string(mode))
}

func (m *MemoryManagerActor) getOrCreateBucket() (nats.KeyValue, error) {
	if m.js == nil {
		return nil, fmt.Errorf("JetStream not available")
	}
	kv, err := m.js.KeyValue(memoryKVBucket)
	if err == nil {
		return kv, nil
	}
	return m.js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:  memoryKVBucket,
		Storage: nats.FileStorage,
	})
}

func (m *MemoryManagerActor) persistToKV(mode msg.ConversationType, state *msg.MemoryState) {
	kv, err := m.getOrCreateBucket()
	if err != nil {
		slog.Error("memory: KV bucket access failed", "err", err)
		return
	}

	data, err := json.Marshal(state)
	if err != nil {
		slog.Error("memory: marshal state", "err", err)
		return
	}

	if _, err := kv.Put(m.kvKey(mode), data); err != nil {
		slog.Error("memory: KV put", "err", err, "key", m.kvKey(mode))
	}
}

func (m *MemoryManagerActor) restoreFromKV() {
	kv, err := m.getOrCreateBucket()
	if err != nil {
		// Bucket might not exist yet — not an error on first run.
		return
	}

	// Try to restore each memory-capable mode.
	modes := []msg.ConversationType{msg.ConvAI, msg.ConvEmail, msg.ConvSlack, msg.ConvChatbot}
	for _, mode := range modes {
		entry, err := kv.Get(m.kvKey(mode))
		if err != nil {
			continue // not found — normal for first run
		}

		var state msg.MemoryState
		if err := json.Unmarshal(entry.Value(), &state); err != nil {
			slog.Warn("memory: unmarshal KV entry", "err", err, "key", m.kvKey(mode))
			continue
		}

		m.states[mode] = &state
		slog.Info("memory: restored from KV", "pane", m.paneID, "mode", mode, "entries", len(state.Entries))
	}
}
