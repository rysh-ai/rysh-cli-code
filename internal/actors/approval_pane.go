// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ApprovalPaneActor is a lightweight ephemeral pane actor that holds approval
// request data for display. It has no PTY, no shell, and no LLM. The TUI
// handles input for approval panes directly.
type ApprovalPaneActor struct {
	id              string
	requestID       string
	sourcePaneID    string
	sourcePaneName  string
	approvalRequest *msg.MsgApprovalRequest
	responseSubject string
	paneGroupID     string
}

// ApprovalPaneConfig holds the configuration for creating an ApprovalPaneActor.
type ApprovalPaneConfig struct {
	ID              string
	RequestID       string
	SourcePaneID    string
	SourcePaneName  string
	ApprovalRequest *msg.MsgApprovalRequest
	ResponseSubject string
	PaneGroupID     string
}

// NewApprovalPaneActor creates a new ApprovalPaneActor from the given config.
func NewApprovalPaneActor(cfg ApprovalPaneConfig) *ApprovalPaneActor {
	return &ApprovalPaneActor{
		id:              cfg.ID,
		requestID:       cfg.RequestID,
		sourcePaneID:    cfg.SourcePaneID,
		sourcePaneName:  cfg.SourcePaneName,
		approvalRequest: cfg.ApprovalRequest,
		responseSubject: cfg.ResponseSubject,
		paneGroupID:     cfg.PaneGroupID,
	}
}

// BuildSnapshot returns a PaneSnapshot representing this approval pane.
// It uses the standard PaneSnapshot type with PaneType="approval" and embeds
// approval-specific data into the existing fields for TUI rendering.
func (a *ApprovalPaneActor) BuildSnapshot() domain.PaneSnapshot {
	title := "Approval"
	if a.sourcePaneName != "" {
		title = "Approval from [" + a.sourcePaneName + "]"
	}

	description := ""
	diffContent := ""
	approvalType := ""

	if a.approvalRequest != nil {
		description = a.approvalRequest.Description
		approvalType = string(a.approvalRequest.Type)
		if a.approvalRequest.Diff != nil {
			diffContent = a.approvalRequest.Diff.UnifiedDiff
		}
	}

	// Build display output for the approval pane.
	output := a.buildDisplayOutput(title, description, diffContent, approvalType)

	return domain.PaneSnapshot{
		ID:       a.id,
		Title:    title,
		PaneType: "approval",
		Mode:     "approval",
		Status:   "waiting_approval",
		Output:   output,
		// Encode requestID and responseSubject in GivenName for TUI access.
		// Format: "requestID\x1FresponseSubject" (unit separator).
		GivenName: a.requestID + "\x1f" + a.responseSubject,
	}
}

// buildDisplayOutput constructs the text content shown in the approval pane.
func (a *ApprovalPaneActor) buildDisplayOutput(title, description, diff, approvalType string) string {
	var out string
	out += " " + title + "\n"
	out += "────────────────────────────────────────────────────────\n"
	out += "\n " + description + "\n\n"

	if diff != "" {
		out += diff + "\n\n"
	}

	out += "────────────────────────────────────────────────────────\n"
	out += " y approve  Y approve-always  n reject  N reject+reason  esc reject\n"

	_ = approvalType
	_ = a.responseSubject
	return out
}

// ApprovalPaneSnapshotData returns the detailed approval pane snapshot for the TUI.
func (a *ApprovalPaneActor) ApprovalPaneSnapshotData() domain.ApprovalPaneSnapshot {
	title := "Approval"
	if a.sourcePaneName != "" {
		title = "Approval from [" + a.sourcePaneName + "]"
	}

	snap := domain.ApprovalPaneSnapshot{
		ID:              a.id,
		PaneType:        "approval",
		RequestID:       a.requestID,
		SourcePaneID:    a.sourcePaneID,
		SourcePaneName:  a.sourcePaneName,
		ResponseSubject: a.responseSubject,
		Title:           title,
	}

	if a.approvalRequest != nil {
		snap.Type = string(a.approvalRequest.Type)
		snap.Description = a.approvalRequest.Description
		if a.approvalRequest.Diff != nil {
			snap.Diff = a.approvalRequest.Diff.UnifiedDiff
			snap.DiffFilePath = a.approvalRequest.Diff.FilePath
		}
	}

	return snap
}
