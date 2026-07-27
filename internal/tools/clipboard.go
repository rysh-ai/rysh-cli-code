package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ClipboardTool reads from and writes to the system clipboard.
type ClipboardTool struct{}

// ClipboardParams holds the parameters for clipboard operations.
type ClipboardParams struct {
	Action  string `json:"action"`            // "read" or "write"
	Content string `json:"content,omitempty"` // content to write (required for action=write)
}

// NewClipboardTool creates a new ClipboardTool.
func NewClipboardTool() *ClipboardTool {
	return &ClipboardTool{}
}

// Spec returns the tool specification for the LLM.
func (t *ClipboardTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["read", "write"], "description": "Whether to read from or write to the clipboard"},
    "content": {"type": "string", "description": "Content to write to clipboard (required when action is 'write')"}
  },
  "required": ["action"]
}`)
	return ToolSpec{
		Name:             "clipboard",
		Description:      "Read from or write to the system clipboard. Useful for sharing data with the user or between panes.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns true for write operations.
func (t *ClipboardTool) RequiresApproval(params json.RawMessage) bool {
	var p ClipboardParams
	if json.Unmarshal(params, &p) == nil && p.Action == "write" {
		return true
	}
	return false
}

// Execute performs the clipboard operation.
func (t *ClipboardTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p ClipboardParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid clipboard params: %w", err)
	}

	switch p.Action {
	case "read":
		return t.read(ctx)
	case "write":
		if p.Content == "" {
			return ErrOutput(ErrKindValidation, "content is required for write action"), nil
		}
		return t.write(ctx, p.Content)
	default:
		return ErrOutputf(ErrKindValidation, "invalid action: %s (must be 'read' or 'write')", p.Action), nil
	}
}

func (t *ClipboardTool) read(ctx context.Context) (*ToolOutput, error) {
	execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(execCtx, "pbpaste")
	case "linux":
		// Try xclip first, then xsel.
		cmd = exec.CommandContext(execCtx, "xclip", "-selection", "clipboard", "-o")
	default:
		return ErrOutputf(ErrKindInternal, "clipboard not supported on %s", runtime.GOOS), nil
	}

	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{buf: &stdout, limit: maxOutputBytes}

	if err := cmd.Run(); err != nil {
		if runtime.GOOS == "linux" {
			// Fallback to xsel.
			cmd = exec.CommandContext(execCtx, "xsel", "--clipboard", "--output")
			stdout.Reset()
			cmd.Stdout = &limitedWriter{buf: &stdout, limit: maxOutputBytes}
			if fallbackErr := cmd.Run(); fallbackErr != nil {
				return ErrOutput(ErrKindMissing, "clipboard read failed: neither xclip nor xsel available"), nil
			}
		} else {
			return ErrOutputf(ErrKindInternal, "clipboard read failed: %v", err), nil
		}
	}

	content := stdout.String()
	if content == "" {
		return &ToolOutput{Content: "(clipboard is empty)"}, nil
	}

	return &ToolOutput{
		Content: content,
		Metadata: map[string]string{
			"length": fmt.Sprintf("%d", len(content)),
		},
	}, nil
}

func (t *ClipboardTool) write(ctx context.Context, content string) (*ToolOutput, error) {
	execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(execCtx, "pbcopy")
	case "linux":
		cmd = exec.CommandContext(execCtx, "xclip", "-selection", "clipboard")
	default:
		return ErrOutputf(ErrKindInternal, "clipboard not supported on %s", runtime.GOOS), nil
	}

	cmd.Stdin = strings.NewReader(content)

	if err := cmd.Run(); err != nil {
		if runtime.GOOS == "linux" {
			cmd = exec.CommandContext(execCtx, "xsel", "--clipboard", "--input")
			cmd.Stdin = strings.NewReader(content)
			if fallbackErr := cmd.Run(); fallbackErr != nil {
				return ErrOutput(ErrKindMissing, "clipboard write failed: neither xclip nor xsel available"), nil
			}
		} else {
			return ErrOutputf(ErrKindInternal, "clipboard write failed: %v", err), nil
		}
	}

	return &ToolOutput{
		Content: fmt.Sprintf("Wrote %d characters to clipboard.", len(content)),
		Metadata: map[string]string{
			"length": fmt.Sprintf("%d", len(content)),
		},
	}, nil
}
