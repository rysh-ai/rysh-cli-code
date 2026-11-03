// SPDX-License-Identifier: Apache-2.0

package msg

import "encoding/json"

// Messages for `rysh mcp serve` (design 067): an outside MCP client driving a
// running session. Reads use the existing snapshot, VT and board-query
// subjects; these two exist for the writes, which must pass the same gates as
// a human-typed command.

// MsgCLIMCPGate asks the WorkspaceActor whether one MCP-initiated write may
// run, and records the attempt in the session's MCP audit log.
//
// The workspace answers from ITS loaded policy (project file merged with the
// org or central file) — the daemon's view, not a copy the MCP process loaded
// on its own. A refusal comes back as MsgCLIResponse{OK:false} naming the rule.
// Nothing is auto-approved and no approval prompt is raised: a gated call
// fails. A daemon that predates this message does not answer it, so the call
// fails too (fail closed).
type MsgCLIMCPGate struct {
	// Tool is the MCP tool name ("send_input"); policy rules see it as
	// "mcp_<tool>", so `approval.always_gate: ["mcp_*"]` gates every MCP write.
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args,omitempty"`
	// Target is a short label for the audit line (pane id, humanoid/channel).
	Target string `json:"target,omitempty"`
	// ShellCommand is set for a shell-mode send_input. It is evaluated as a
	// bash call as well, so bash.deny rules apply to what would be typed.
	ShellCommand string `json:"shell_command,omitempty"`
	// Caller is always "mcp"; it is carried so the audit line says who asked.
	Caller string `json:"caller"`
}

// MsgHumanoidMCPSend asks a humanoid to send one message on one of its
// channels on behalf of an MCP client. The humanoid applies the channel's
// ai|human governance: in human mode the message becomes a pending draft the
// owner approves by typing "send" in the humanoid's pane; it is never sent.
// Sent as a request to rysh.humanoid.{name}.inbox; answered with
// MsgHumanoidMCPSendReply.
type MsgHumanoidMCPSend struct {
	ChannelType string `json:"channel_type"`
	RecipientID string `json:"recipient_id"`
	Content     string `json:"content"`
	ThreadID    string `json:"thread_id,omitempty"`
}

// MsgHumanoidMCPSendReply says what happened: "sent", "drafted" (human
// governance; nothing left rysh), or an error.
type MsgHumanoidMCPSendReply struct {
	Outcome    string `json:"outcome,omitempty"`
	Governance string `json:"governance,omitempty"`
	Queued     int    `json:"queued,omitempty"` // drafts awaiting the owner, this one included
	Error      string `json:"error,omitempty"`
}
