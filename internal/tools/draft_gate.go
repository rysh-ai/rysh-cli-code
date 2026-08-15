// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// requireApprovedDraft is THE draft-and-confirm gate shared by every
// tool-governed channel send tool (slack_send, email_send, whatsapp_send).
// It returns nil when the send may proceed, or a model-visible refusal that
// the caller must return as the tool output.
//
// The gate is enforced HERE, in the tools, rather than through the approval
// plumbing on purpose: a humanoid may legitimately run headless with
// auto-approve on (nobody is at the keyboard), which short-circuits the
// orchestrator's approval check before RequiresApproval is ever consulted.
// Governance that depends on a flag another code path can switch off is not
// governance — and the system prompt telling the model "do not send unbidden"
// is an instruction, not an enforcement point. Slack learned this as defect 9;
// email and whatsapp regressed the same way (design 019 gap 1), which is why
// all three now share this one implementation — one place to audit.
//
// humanGoverned is read PER CALL (never captured at construction) so a
// runtime `##humanoid governance <name> ai|human` takes effect on the very
// next tool call. nil means "never human-governed" (non-humanoid callers).
//
// In human mode the ONLY payload that reaches the channel is a draft the
// owner explicitly confirmed — Draft.ApprovedAt non-zero, per the contract in
// internal/channels/draft_store.go.
func requireApprovedDraft(
	humanGoverned func() bool,
	drafts *channels.DraftStore,
	draftID, sendTool, draftTool string,
) *ToolOutput {
	if humanGoverned == nil || !humanGoverned() {
		return nil // ai mode, or a non-humanoid caller: ungated
	}
	if draftID == "" {
		return ErrOutputf(ErrKindValidation,
			"human governance is on: %s requires the draft_id of an owner-approved draft. "+
				"Call %s, show it to the owner, and wait for their confirmation — "+
				"a direct send is not permitted in this mode.", sendTool, draftTool)
	}
	if drafts == nil {
		// No draft store, so no draft can ever have been owner-approved.
		// Fail closed instead of waving the send through (or panicking).
		return ErrOutputf(ErrKindValidation,
			"human governance is on but %s has no draft store to verify an owner "+
				"approval against — refusing to send.", sendTool)
	}
	d, ok := drafts.Get(draftID)
	if !ok {
		return ErrOutputf(ErrKindMissing, "draft %q not found", draftID)
	}
	if !d.Approved() {
		return ErrOutputf(ErrKindValidation,
			"draft %q has not been approved by the owner yet — it stays unsent until they "+
				"confirm it in the humanoid's pane.", draftID)
	}
	return nil
}
