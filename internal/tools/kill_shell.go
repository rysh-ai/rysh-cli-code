// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// KillShellTool terminates a background shell session.
type KillShellTool struct {
	mgr *BackgroundSessionManager
}

// KillShellParams holds the parameters for killing a background session.
type KillShellParams struct {
	SessionID string `json:"session_id"`
}

// NewKillShellTool creates a KillShellTool.
func NewKillShellTool(mgr *BackgroundSessionManager) *KillShellTool {
	return &KillShellTool{mgr: mgr}
}

// Spec returns the tool specification for the LLM.
func (t *KillShellTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "session_id": {"type": "string", "description": "The background session ID to terminate"}
  },
  "required": ["session_id"]
}`)
	return ToolSpec{
		Name:             "kill_shell",
		Description:      "Terminate a background shell session started with bash_background.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval always returns true — killing processes is significant.
func (t *KillShellTool) RequiresApproval(_ json.RawMessage) bool {
	return true
}

// Execute kills the background session.
func (t *KillShellTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p KillShellParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid kill_shell params: %w", err)
	}

	if p.SessionID == "" {
		return ErrOutput(ErrKindValidation, "session_id is required"), nil
	}

	// Get final output before killing.
	output, _, _ := t.mgr.ReadOutput(p.SessionID, 4096)

	if err := t.mgr.Kill(p.SessionID); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to kill session %s: %v", p.SessionID, err), nil
	}

	// Remove from the manager.
	t.mgr.Remove(p.SessionID)

	content := fmt.Sprintf("Session %s terminated.", p.SessionID)
	if output != "" {
		content += fmt.Sprintf("\nFinal output:\n%s", output)
	}

	return &ToolOutput{
		Content: content,
		Metadata: map[string]string{
			"session_id": p.SessionID,
		},
	}, nil
}
