// Autonomous agent messages.
package msg

// ---------------------------------------------------------------------------
// Autonomous Agent messages
// ---------------------------------------------------------------------------

// MsgAgentCreate creates a new autonomous agent. WorktreeBranch/WorktreePath
// record the agent's isolation worktree when one was provisioned at spawn
// (`isolation: worktree` frontmatter or `##agent spawn --worktree`, design
// 008); empty means the agent works on the shared checkout.
type MsgAgentCreate struct {
	Name           string `json:"name"`
	SystemPrompt   string `json:"system_prompt"`
	WorktreeBranch string `json:"worktree_branch,omitempty"`
	WorktreePath   string `json:"worktree_path,omitempty"`
	// AutoApprove carries the skill-file `auto_approve:` field. Nil = absent =
	// the default, TRUE: an agent runs autonomously with no terminal, so a
	// gated tool call would block on a dialog nobody can answer.
	AutoApprove *bool `json:"auto_approve,omitempty"`
}

// MsgAgentDelete deletes an agent by name.
type MsgAgentDelete struct {
	Name string `json:"name"`
}

// MsgAgentStop interrupts an agent's in-flight LLM run by name (used by
// @@agent-name stop). PAUSE semantics: the agent stays alive and its
// conversation state / session memory are preserved — MsgAgentContinue (or a
// new prompt) resumes from the checkpoint. Deleting an agent is a separate
// operation (MsgAgentDelete / ##agent delete).
type MsgAgentStop struct {
	Name string `json:"name"`
}

// MsgAgentContinue resumes an agent's paused LLM run by name (used by
// @@agent-name continue). No-op with a notice when nothing is paused.
type MsgAgentContinue struct {
	Name string `json:"name"`
}

// MsgAgentActivate activates a deactivated agent.
type MsgAgentActivate struct {
	Name string `json:"name"`
}

// MsgAgentDeactivate deactivates an agent (keeps state, stops processing).
type MsgAgentDeactivate struct {
	Name string `json:"name"`
}

// MsgAgentList requests a list of all agents.
type MsgAgentList struct{}

// MsgAgentListReply carries the list of agents.
type MsgAgentListReply struct {
	Agents []AgentInfo `json:"agents"`
}

// AgentInfo describes an agent's state.
type AgentInfo struct {
	Name            string   `json:"name"`
	Active          bool     `json:"active"`
	SystemPrompt    string   `json:"system_prompt"`
	RegisteredPanes []string `json:"registered_panes,omitempty"`
	// WorktreePath is the agent's isolation worktree (design 008), "" when the
	// agent runs on the shared checkout. Surfaced so the worktree lifecycle can
	// refuse to auto-remove a worktree a live agent still claims.
	WorktreePath string `json:"worktree_path,omitempty"`
}

// MsgAgentPrompt sends a prompt to a named agent.
type MsgAgentPrompt struct {
	AgentName    string `json:"agent_name"`
	Prompt       string `json:"prompt"`
	SourcePaneID string `json:"source_pane_id"`
	ScopeHint    string `json:"scope_hint,omitempty"` // invoking pane's scope chain (resolved by the workspace)
}

// MsgAgentRegisterPane registers an agent to output to a specific pane.
type MsgAgentRegisterPane struct {
	AgentName string `json:"agent_name"`
	PaneID    string `json:"pane_id"`
	PaneName  string `json:"pane_name"`
}

// MsgAgentUnregisterPane removes an agent's pane registration.
type MsgAgentUnregisterPane struct {
	AgentName string `json:"agent_name"`
	PaneID    string `json:"pane_id"`
}
