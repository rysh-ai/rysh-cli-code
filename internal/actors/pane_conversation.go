package actors

// pane_conversation.go — Unified ConversationMessage handlers for PaneActor.
// These handlers accept the new MsgConversationAppend and MsgConversationHistoryAppend
// messages and route them to both the structured conversation buffers AND the
// legacy string buffers (for backward compatibility during migration).

import (
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Per-mode conversation turn limits (message count, not byte count).
// These replace the old maxPaneBuffer byte cap for structured messages.
// ---------------------------------------------------------------------------

const (
	shellMaxTurns   = 200
	aiMaxTurns      = 100
	ryshMaxTurns    = 50
	chatMaxTurns    = 200
	emailMaxTurns   = 100
	slackMaxTurns   = 100
	chatbotMaxTurns = 100

	// mergedMaxMessages is the cap for the merged (shell+ai) buffer.
	mergedMaxMessages = 300
)

// turnLimit returns the max turn count for a conversation type.
func turnLimit(ct msg.ConversationType) int {
	switch ct {
	case msg.ConvShell:
		return shellMaxTurns
	case msg.ConvAI:
		return aiMaxTurns
	case msg.ConvRysh:
		return ryshMaxTurns
	case msg.ConvChat:
		return chatMaxTurns
	case msg.ConvEmail:
		return emailMaxTurns
	case msg.ConvSlack:
		return slackMaxTurns
	case msg.ConvChatbot:
		return chatbotMaxTurns
	default:
		return 100
	}
}

// ---------------------------------------------------------------------------
// Conversation output handler
// ---------------------------------------------------------------------------

// handleConversationAppend routes a unified ConversationMessage to the correct
// structured buffer AND the legacy string buffer (for backward compatibility).
func (p *PaneActor) handleConversationAppend(cm *msg.ConversationMessage) {
	if cm == nil {
		return
	}

	// 1. Append to structured per-mode buffer.
	ct := cm.ConversationType
	buf := append(p.conversations[ct], cm)
	evicted := evictConversationMessages(&buf, turnLimit(ct))
	p.conversations[ct] = buf

	// 2. If messages were evicted from a memory-capable mode, trigger summarization.
	if len(evicted) > 0 && ct.HasMemory() {
		p.triggerMemorySummarization(ct, evicted)
	}

	// 3. Dual-publish modes also append to merged.
	if ct.IsDualPublish() {
		p.mergedConversation = append(p.mergedConversation, cm)
		evictConversationMessages(&p.mergedConversation, mergedMaxMessages)
	}

	// 3. Bridge to legacy string buffers so existing TUI rendering works.
	text := cm.Content
	switch ct {
	case msg.ConvShell:
		p.appendShellOutput(text)
	case msg.ConvAI:
		p.appendAIOutput(text)
	case msg.ConvRysh:
		p.AppendRyshOutput(text)
	case msg.ConvChat:
		p.appendChatOutput(text)
	case msg.ConvEmail, msg.ConvSlack, msg.ConvChatbot:
		p.appendExternalOutput(text)
	default:
		// Dynamic per-mode buffer (e.g. a humanoid's own mode named after it).
		p.appendModeOutput(string(ct), text)
	}

	// 4. For dual-publish types, also append to merged legacy buffer.
	if ct.IsDualPublish() {
		p.appendOutput(text)
		p.appendPrivateBuffer(text)
	} else if p.remoteSubscriber && isVisibleSubscriberMode(ct) {
		// On a remote-share subscriber pane, fold non-shell remote modes
		// (chat/rysh/external) into the merged display buffer too, prefixed with
		// the mode, so a passive viewer sees them in the default view without
		// switching the pane's input mode. The per-mode buffers above still hold
		// the unprefixed text for the dedicated mode views.
		p.appendOutput("[" + string(ct) + "] " + text)
	}

	p.kvDirty = true
}

// isVisibleSubscriberMode reports whether a non-dual-publish conversation type
// should be surfaced in a remote subscriber pane's merged view.
func isVisibleSubscriberMode(ct msg.ConversationType) bool {
	switch ct {
	case msg.ConvChat, msg.ConvRysh, msg.ConvEmail, msg.ConvSlack, msg.ConvChatbot:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Conversation history handler
// ---------------------------------------------------------------------------

// handleConversationHistoryAppend routes a unified ConversationMessage to the
// correct structured history buffer AND the legacy string history array.
func (p *PaneActor) handleConversationHistoryAppend(cm *msg.ConversationMessage) {
	if cm == nil {
		return
	}

	// 1. Append to structured per-mode history buffer.
	ct := cm.ConversationType
	hbuf := append(p.convHistories[ct], cm)
	evictConversationMessages(&hbuf, turnLimit(ct))
	p.convHistories[ct] = hbuf

	// 2. Dual-publish modes also append to merged history.
	if ct.IsDualPublish() {
		p.mergedConvHistory = append(p.mergedConvHistory, cm)
		evictConversationMessages(&p.mergedConvHistory, mergedMaxMessages)
	}

	// 3. Bridge to legacy string history arrays.
	entry := cm.Content
	switch ct {
	case msg.ConvShell:
		p.shellHistory = append(p.shellHistory, entry)
	case msg.ConvAI:
		p.promptHistory = append(p.promptHistory, entry)
	case msg.ConvRysh:
		p.ryshHistory = append(p.ryshHistory, entry)
	case msg.ConvChat:
		p.chatHistory = append(p.chatHistory, entry)
	case msg.ConvEmail, msg.ConvSlack, msg.ConvChatbot:
		p.externalHistory = append(p.externalHistory, entry)
	}

	// 4. For dual-publish types, also append to merged legacy history.
	if ct.IsDualPublish() {
		p.mergedHistory = append(p.mergedHistory, entry)
	}

	p.kvDirty = true

	// A history append isn't a structural layout change, so snapshot-driven
	// clients (the desktop app's ?stream=1 channel and the TUI's event-driven
	// layout fetch) would otherwise never re-fetch a snapshot — which left the
	// up/down-arrow command recall stale: the app never saw new entries until
	// an unrelated layout event fired, and the TUI lagged up to a full
	// reconcile tick. Command history lives in the layout-only snapshot (there
	// is no separate content stream for it), so signal a refresh here. Fires
	// once per submitted command — cheap, and coalesced client-side.
	p.notifyLayoutDirty()
}

// ---------------------------------------------------------------------------
// Turn-based eviction
// ---------------------------------------------------------------------------

// evictConversationMessages drops the oldest messages when the buffer exceeds
// the limit. This is message-count based (not turn-count) for simplicity —
// each ConversationMessage counts as 1. A turn (question+answer) counts as 2
// messages, so the effective turn limit is roughly limit/2.
//
// Returns the evicted messages (for memory summarization) or nil if no eviction.
func evictConversationMessages(buf *[]*msg.ConversationMessage, limit int) []*msg.ConversationMessage {
	if limit <= 0 || len(*buf) <= limit {
		return nil
	}
	// Evict the oldest messages.
	excess := len(*buf) - limit
	evicted := make([]*msg.ConversationMessage, excess)
	copy(evicted, (*buf)[:excess])
	// Keep the last `limit` messages.
	*buf = (*buf)[excess:]
	return evicted
}

// ---------------------------------------------------------------------------
// Memory summarization trigger
// ---------------------------------------------------------------------------

// triggerMemorySummarization sends evicted messages to the MemoryManager for
// LLM-based summarization. Only called for memory-capable modes (AI, Email,
// Slack, Chatbot). The call is non-blocking — summarization runs async.
func (p *PaneActor) triggerMemorySummarization(ct msg.ConversationType, evicted []*msg.ConversationMessage) {
	// Convert pointer slice to value slice for the message.
	turns := make([]msg.ConversationMessage, len(evicted))
	for i, m := range evicted {
		turns[i] = *m
	}
	// Publish summarization request to the MemoryManager via NATS.
	_ = p.pub.SendMemorySummarize(p.id, ct, turns)
}

// ---------------------------------------------------------------------------
// Active turn tracking
// ---------------------------------------------------------------------------

// SetActiveTurn records the active turn ID for a conversation type.
// Used when starting a new shell command or AI prompt to track which
// turn subsequent streaming answers belong to.
func (p *PaneActor) SetActiveTurn(ct msg.ConversationType, turnID string) {
	p.activeTurnIDs[ct] = turnID
}

// ActiveTurn returns the active turn ID for a conversation type.
func (p *PaneActor) ActiveTurn(ct msg.ConversationType) string {
	return p.activeTurnIDs[ct]
}

// restoreConversations restores structured conversation buffers from a
// PaneSnapshot. Called during session restore.
func (p *PaneActor) restoreConversations(snap domain.PaneSnapshot) {
	// Restore per-mode conversations.
	if snap.Conversations != nil {
		for mode, msgs := range snap.Conversations {
			ct := msg.ConversationType(mode)
			restored := make([]*msg.ConversationMessage, len(msgs))
			for i, m := range msgs {
				cm := snapshotToConvMsg(m)
				restored[i] = &cm
			}
			p.conversations[ct] = restored
		}
	}
	// Restore merged conversations.
	if snap.MergedConv != nil {
		p.mergedConversation = make([]*msg.ConversationMessage, len(snap.MergedConv))
		for i, m := range snap.MergedConv {
			cm := snapshotToConvMsg(m)
			p.mergedConversation[i] = &cm
		}
	}
	// Restore per-mode history.
	if snap.ConvHistories != nil {
		for mode, msgs := range snap.ConvHistories {
			ct := msg.ConversationType(mode)
			restored := make([]*msg.ConversationMessage, len(msgs))
			for i, m := range msgs {
				cm := snapshotToConvMsg(m)
				restored[i] = &cm
			}
			p.convHistories[ct] = restored
		}
	}
	// Restore merged history.
	if snap.MergedConvHistory != nil {
		p.mergedConvHistory = make([]*msg.ConversationMessage, len(snap.MergedConvHistory))
		for i, m := range snap.MergedConvHistory {
			cm := snapshotToConvMsg(m)
			p.mergedConvHistory[i] = &cm
		}
	}
}

// snapshotToConvMsg converts a domain snapshot representation back to a
// msg.ConversationMessage.
func snapshotToConvMsg(s domain.ConversationMessageSnapshot) msg.ConversationMessage {
	return msg.ConversationMessage{
		TurnID:           s.TurnID,
		TurnType:         msg.TurnType(s.TurnType),
		ConversationType: msg.ConversationType(s.ConversationType),
		InputType:        msg.InputType(s.InputType),
		MessageSource:    msg.MessageSource(s.MessageSource),
		Content:          s.Content,
		TimestampMs:      s.TimestampMs,
		Sensitive:        s.Sensitive,
		SubjectToShare:   s.SubjectToShare,
		Role:             s.Role,
		Streaming:        s.Streaming,
	}
}

// ---------------------------------------------------------------------------
// Snapshot conversion helpers
// ---------------------------------------------------------------------------

// snapshotConversations converts the structured per-mode conversation buffers
// into domain snapshot format for the TUI.
func (p *PaneActor) snapshotConversations() map[string][]domain.ConversationMessageSnapshot {
	if len(p.conversations) == 0 {
		return nil
	}
	result := make(map[string][]domain.ConversationMessageSnapshot, len(p.conversations))
	for ct, msgs := range p.conversations {
		if len(msgs) > 0 {
			result[string(ct)] = convertConvMsgs(msgs)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// snapshotModeOutputs converts the dynamic per-mode buffers (per-humanoid modes)
// into a map of mode name → text for the snapshot. Returns nil when empty.
func (p *PaneActor) snapshotModeOutputs() map[string]string {
	if len(p.modeOutputs) == 0 {
		return nil
	}
	out := make(map[string]string, len(p.modeOutputs))
	for mode, b := range p.modeOutputs {
		if b != nil && b.Len() > 0 {
			out[mode] = strings.TrimRight(b.String(), "\n")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// restoreModeOutputs rebuilds the dynamic per-mode buffers from a snapshot.
func (p *PaneActor) restoreModeOutputs(m map[string]string) {
	if len(m) == 0 {
		return
	}
	if p.modeOutputs == nil {
		p.modeOutputs = make(map[string]*cappedBuffer, len(m))
	}
	for mode, s := range m {
		b := &cappedBuffer{}
		b.Set(s)
		p.modeOutputs[mode] = b
	}
}

// snapshotConvHistories converts the structured per-mode history buffers
// into domain snapshot format.
func (p *PaneActor) snapshotConvHistories() map[string][]domain.ConversationMessageSnapshot {
	if len(p.convHistories) == 0 {
		return nil
	}
	result := make(map[string][]domain.ConversationMessageSnapshot, len(p.convHistories))
	for ct, msgs := range p.convHistories {
		if len(msgs) > 0 {
			result[string(ct)] = convertConvMsgs(msgs)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// convertConvMsgs converts a slice of msg.ConversationMessage pointers into
// domain snapshot structs.
func convertConvMsgs(msgs []*msg.ConversationMessage) []domain.ConversationMessageSnapshot {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]domain.ConversationMessageSnapshot, len(msgs))
	for i, m := range msgs {
		out[i] = domain.ConversationMessageSnapshot{
			TurnID:           m.TurnID,
			TurnType:         string(m.TurnType),
			ConversationType: string(m.ConversationType),
			InputType:        string(m.InputType),
			MessageSource:    string(m.MessageSource),
			Content:          m.Content,
			TimestampMs:      m.TimestampMs,
			Sensitive:        m.Sensitive,
			SubjectToShare:   m.SubjectToShare,
			Role:             m.Role,
			Streaming:        m.Streaming,
		}
	}
	return out
}
