// SPDX-License-Identifier: Apache-2.0

package domain

// "Needs you" attention (root-order-044 #3).
//
// A pane needs you when something in it is waiting on a human: a tool call
// held at the approval gate, an ask_user question, or an agent CLI (claude,
// codex, gemini) showing a permission or question prompt. The pane actor
// OBSERVES this; nothing declares it. The keys below are added to the pane's
// meta in live snapshots only. They are never written to the durable meta or
// to a persisted snapshot, because a "needs you" that survives a restart is a
// stale one.
const (
	PaneMetaAttention       = "attention"
	PaneMetaAttentionReason = "attention.reason"
	PaneMetaAttentionSince  = "attention.since" // unix millis

	// AttentionNeedsYou is the only value of PaneMetaAttention.
	AttentionNeedsYou = "needs_you"

	AttentionReasonApproval = "approval" // a tool call held at the approval gate
	AttentionReasonQuestion = "question" // ask_user is waiting for an answer
	AttentionReasonPrompt   = "prompt"   // an agent CLI's prompt, seen on screen
)

// PaneNeedsYou reports whether meta says the pane needs you, and why. Only the
// exact value counts: anything else is "no".
func PaneNeedsYou(meta map[string]string) (reason string, ok bool) {
	if meta[PaneMetaAttention] != AttentionNeedsYou {
		return "", false
	}
	return meta[PaneMetaAttentionReason], true
}

// IsAttentionMetaKey reports whether k is one of the attention keys.
func IsAttentionMetaKey(k string) bool {
	switch k {
	case PaneMetaAttention, PaneMetaAttentionReason, PaneMetaAttentionSince:
		return true
	}
	return false
}
