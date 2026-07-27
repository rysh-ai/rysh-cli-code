package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// BashSessionTool unifies the former bash_output and kill_shell tools behind a
// single `action` switch (read | list | kill) for managing background bash
// sessions started by bash_background. It delegates to the underlying
// executors so session logic lives in one place.
type BashSessionTool struct {
	output *BashOutputTool
	kill   *KillShellTool
}

// NewBashSessionTool builds a BashSessionTool backed by the given session manager.
func NewBashSessionTool(mgr *BackgroundSessionManager) *BashSessionTool {
	return &BashSessionTool{
		output: NewBashOutputTool(mgr),
		kill:   NewKillShellTool(mgr),
	}
}

// Spec returns the unified tool specification for the LLM.
func (t *BashSessionTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["read", "list", "kill"], "description": "read=read a session's output; list=list active sessions; kill=terminate a session"},
    "session_id": {"type": "string", "description": "Session ID (required for read/kill; omit for list)"},
    "tail_bytes": {"type": "integer", "description": "read action: return only the last N bytes of output (default: all)"}
  },
  "required": ["action"]
}`)
	return ToolSpec{
		Name:             "bash_session",
		Description:      "Manage background bash sessions started by bash_background. action=read reads a session's output; action=list lists active sessions; action=kill terminates a session.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval is always false — session inspection/termination is low-risk
// and bounded to sessions this process started.
func (t *BashSessionTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute dispatches to the output or kill executor based on `action`.
func (t *BashSessionTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p struct {
		Action    string `json:"action"`
		SessionID string `json:"session_id,omitempty"`
		TailBytes int    `json:"tail_bytes,omitempty"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid bash_session params: %w", err)
	}

	switch p.Action {
	case "read":
		sub, _ := json.Marshal(BashOutputParams{SessionID: p.SessionID, TailBytes: p.TailBytes})
		return t.output.Execute(ctx, sub)
	case "list":
		// An empty session_id makes bash_output list all sessions.
		sub, _ := json.Marshal(BashOutputParams{})
		return t.output.Execute(ctx, sub)
	case "kill":
		sub, _ := json.Marshal(KillShellParams{SessionID: p.SessionID})
		return t.kill.Execute(ctx, sub)
	default:
		return ErrOutputf(ErrKindValidation, "unknown action %q (use read|list|kill)", p.Action), nil
	}
}
