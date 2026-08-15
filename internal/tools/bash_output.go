// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// BashOutputTool reads output from a background shell session.
type BashOutputTool struct {
	mgr *BackgroundSessionManager
}

// BashOutputParams holds the parameters for reading background output.
type BashOutputParams struct {
	SessionID string `json:"session_id"`           // session to read from; empty lists all
	TailBytes int    `json:"tail_bytes,omitempty"` // read only the last N bytes of output (default: all)
}

// NewBashOutputTool creates a BashOutputTool.
func NewBashOutputTool(mgr *BackgroundSessionManager) *BashOutputTool {
	return &BashOutputTool{mgr: mgr}
}

// Spec returns the tool specification for the LLM.
func (t *BashOutputTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "session_id": {"type": "string", "description": "The background session ID returned by bash_background. If omitted, lists all active sessions."},
    "tail_bytes": {"type": "integer", "description": "Return only the last N bytes of output. Default: return all buffered output (up to 256KB)."}
  }
}`)
	return ToolSpec{
		Name:             "bash_output",
		Description:      "Read output from a background shell session started with bash_background. If session_id is omitted, lists all active background sessions.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false — read-only.
func (t *BashOutputTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute reads the output or lists sessions.
func (t *BashOutputTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p BashOutputParams
	if params != nil {
		_ = json.Unmarshal(params, &p)
	}

	// If no session ID, list all sessions.
	if p.SessionID == "" {
		return t.listSessions()
	}

	output, finished, err := t.mgr.ReadOutput(p.SessionID, p.TailBytes)
	if err != nil {
		return ErrOutput(ErrKindInternal, err.Error()), nil
	}

	status := "running"
	if finished {
		sess, ok := t.mgr.Get(p.SessionID)
		if ok {
			sess.mu.Lock()
			status = fmt.Sprintf("exited (code %d)", sess.exitCode)
			sess.mu.Unlock()
		}
	}

	return &ToolOutput{
		Content: fmt.Sprintf("[session: %s, status: %s]\n%s", p.SessionID, status, output),
		Metadata: map[string]string{
			"session_id": p.SessionID,
			"status":     status,
		},
	}, nil
}

func (t *BashOutputTool) listSessions() (*ToolOutput, error) {
	infos := t.mgr.List()
	if len(infos) == 0 {
		return &ToolOutput{Content: "No active background sessions."}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Active background sessions (%d):\n", len(infos)))
	for _, info := range infos {
		status := "running"
		if info.Finished {
			status = fmt.Sprintf("exited (code %d)", info.ExitCode)
		}
		sb.WriteString(fmt.Sprintf("  %s  pid:%d  %s  %s\n",
			info.ID, info.PID, status, info.Command))
	}
	return &ToolOutput{Content: sb.String()}, nil
}
