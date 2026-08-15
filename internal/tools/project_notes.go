// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	notesFileName = ".rysh-notes.md"
	maxNotesSize  = 100 * 1024 // 100KB
)

// ProjectNotesTool manages project-level notes shared across panes.
type ProjectNotesTool struct {
	workDir string
}

// ProjectNotesParams holds the parameters for project notes operations.
type ProjectNotesParams struct {
	Action  string `json:"action"`            // "read", "append", "write", "clear"
	Content string `json:"content,omitempty"` // content for append/write
	Section string `json:"section,omitempty"` // section heading for append
}

// NewProjectNotesTool creates a ProjectNotesTool with the given working directory.
func NewProjectNotesTool(workDir string) *ProjectNotesTool {
	return &ProjectNotesTool{workDir: workDir}
}

// Spec returns the tool specification for the LLM.
func (t *ProjectNotesTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["read", "append", "write", "clear"],
      "description": "Operation: read (view notes), append (add to notes), write (replace notes), clear (delete notes)"
    },
    "content": {"type": "string", "description": "Content to write or append (required for append/write)"},
    "section": {"type": "string", "description": "Section heading when appending (e.g., 'Architecture Decisions')"}
  },
  "required": ["action"]
}`)
	return ToolSpec{
		Name:             "project_notes",
		Description:      "Manage project-level notes (.rysh-notes.md) shared across all panes. Use for decisions, findings, and shared context.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval returns true for write/append/clear actions.
func (t *ProjectNotesTool) RequiresApproval(params json.RawMessage) bool {
	var p ProjectNotesParams
	if json.Unmarshal(params, &p) == nil {
		return p.Action == "write" || p.Action == "append" || p.Action == "clear"
	}
	return true
}

// Execute performs the notes operation.
func (t *ProjectNotesTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p ProjectNotesParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid project_notes params: %w", err)
	}

	notesPath := filepath.Join(t.workDir, notesFileName)

	switch p.Action {
	case "read":
		return t.readNotes(notesPath)
	case "append":
		if p.Content == "" {
			return ErrOutput(ErrKindValidation, "content is required for append action"), nil
		}
		return t.appendNotes(notesPath, p.Content, p.Section)
	case "write":
		if p.Content == "" {
			return ErrOutput(ErrKindValidation, "content is required for write action"), nil
		}
		return t.writeNotes(notesPath, p.Content)
	case "clear":
		return t.clearNotes(notesPath)
	default:
		return ErrOutputf(ErrKindValidation, "invalid action: %s", p.Action), nil
	}
}

func (t *ProjectNotesTool) readNotes(path string) (*ToolOutput, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ToolOutput{Content: "(no project notes yet — use action 'append' or 'write' to create)"}, nil
		}
		return ErrOutputf(ErrKindInternal, "failed to read notes: %v", err), nil
	}

	content := string(data)
	if content == "" {
		return &ToolOutput{Content: "(project notes file is empty)"}, nil
	}

	return &ToolOutput{
		Content: content,
		Metadata: map[string]string{
			"path": path,
			"size": fmt.Sprintf("%d", len(data)),
		},
	}, nil
}

func (t *ProjectNotesTool) appendNotes(path, content, section string) (*ToolOutput, error) {
	// Read existing content.
	existing, _ := os.ReadFile(path)
	if len(existing)+len(content) > maxNotesSize {
		return ErrOutputf(ErrKindValidation, "notes would exceed maximum size (%d bytes)", maxNotesSize), nil
	}

	var sb strings.Builder
	sb.Write(existing)

	// Add separator if there's existing content.
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n\n") {
		if !strings.HasSuffix(string(existing), "\n") {
			sb.WriteByte('\n')
		}
		sb.WriteByte('\n')
	}

	// Add section heading if provided.
	if section != "" {
		sb.WriteString(fmt.Sprintf("## %s\n\n", section))
	}

	// Add timestamp.
	sb.WriteString(fmt.Sprintf("_(%s)_\n\n", time.Now().Format("2006-01-02 15:04")))
	sb.WriteString(content)
	sb.WriteByte('\n')

	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to write notes: %v", err), nil
	}

	return &ToolOutput{
		Content: fmt.Sprintf("Appended to project notes (%d chars added).", len(content)),
		Metadata: map[string]string{
			"path": path,
		},
	}, nil
}

func (t *ProjectNotesTool) writeNotes(path, content string) (*ToolOutput, error) {
	if len(content) > maxNotesSize {
		return ErrOutputf(ErrKindValidation, "content exceeds maximum size (%d bytes)", maxNotesSize), nil
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to write notes: %v", err), nil
	}

	return &ToolOutput{
		Content: fmt.Sprintf("Project notes written (%d chars).", len(content)),
		Metadata: map[string]string{
			"path": path,
		},
	}, nil
}

func (t *ProjectNotesTool) clearNotes(path string) (*ToolOutput, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return ErrOutputf(ErrKindInternal, "failed to clear notes: %v", err), nil
	}

	return &ToolOutput{Content: "Project notes cleared."}, nil
}
