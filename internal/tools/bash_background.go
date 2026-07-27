package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/google/uuid"
)

// BashBackgroundTool starts a shell command in the background.
type BashBackgroundTool struct {
	workDir string
	mgr     *BackgroundSessionManager
}

// BashBackgroundParams holds the parameters for starting a background command.
type BashBackgroundParams struct {
	Command string `json:"command"`
	WorkDir string `json:"work_dir,omitempty"`
}

// NewBashBackgroundTool creates a BashBackgroundTool.
func NewBashBackgroundTool(workDir string, mgr *BackgroundSessionManager) *BashBackgroundTool {
	return &BashBackgroundTool{workDir: workDir, mgr: mgr}
}

// Spec returns the tool specification for the LLM.
func (t *BashBackgroundTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "The bash command to run in the background (e.g., 'npm run dev', 'go test -v ./...', 'tail -f /var/log/app.log')"},
    "work_dir": {"type": "string", "description": "Working directory override"}
  },
  "required": ["command"]
}`)
	return ToolSpec{
		Name:             "bash_background",
		Description:      "Start a shell command in the background and return a session ID. Use bash_output to read its output and kill_shell to stop it. Ideal for dev servers, file watchers, and long-running test suites.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval always returns true — executing commands needs approval.
func (t *BashBackgroundTool) RequiresApproval(_ json.RawMessage) bool {
	return true
}

// Execute starts the background command.
func (t *BashBackgroundTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p BashBackgroundParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid bash_background params: %w", err)
	}

	if p.Command == "" {
		return ErrOutput(ErrKindValidation, "command is required"), nil
	}

	workDir := t.workDir
	if p.WorkDir != "" {
		if filepath.IsAbs(p.WorkDir) {
			workDir = p.WorkDir
		} else {
			workDir = filepath.Join(t.workDir, p.WorkDir)
		}
	}

	sessionID := uuid.New().String()[:8]

	if err := t.mgr.Start(sessionID, p.Command, workDir); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to start background command: %v", err), nil
	}

	return &ToolOutput{
		Content: fmt.Sprintf("Background session started: %s\nCommand: %s\nUse bash_output(session_id: \"%s\") to read output.\nUse kill_shell(session_id: \"%s\") to stop it.", sessionID, p.Command, sessionID, sessionID),
		Metadata: map[string]string{
			"session_id": sessionID,
			"command":    p.Command,
		},
	}, nil
}
