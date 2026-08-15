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
	memoryFileName = "RYSH.md"
	maxMemorySize  = 64 * 1024 // 64KB
)

// MemoryEditTool lets the assistant persist durable facts (project conventions,
// user preferences, decisions) to the project memory file RYSH.md. Unlike the
// ephemeral context_store KV, this file is committed and auto-loaded into the
// system prompt on every session.
type MemoryEditTool struct {
	workDir string
}

// MemoryEditParams holds the parameters for memory operations.
type MemoryEditParams struct {
	Action  string `json:"action"`            // "read", "append", "write"
	Content string `json:"content,omitempty"` // content for append/write
	Section string `json:"section,omitempty"` // optional heading for append
}

// NewMemoryEditTool creates a MemoryEditTool rooted at workDir.
func NewMemoryEditTool(workDir string) *MemoryEditTool {
	return &MemoryEditTool{workDir: workDir}
}

// Spec returns the tool specification for the LLM.
func (t *MemoryEditTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["read", "append", "write"],
      "description": "read (view memory), append (add a durable note), write (replace entire memory file)"
    },
    "content": {"type": "string", "description": "Content to append or write (required for append/write)"},
    "section": {"type": "string", "description": "Optional section heading when appending (e.g., 'Conventions')"}
  },
  "required": ["action"]
}`)
	return ToolSpec{
		Name: "memory_edit",
		Description: "Persist durable project memory to RYSH.md (auto-loaded into the system prompt on every session). " +
			"Use it to record stable facts: build/test commands, code conventions, architecture decisions, and user preferences. " +
			"Do NOT use it for transient task state (use context_store/todo for that).",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval asks for confirmation before replacing the whole file;
// appends and reads are allowed without approval.
func (t *MemoryEditTool) RequiresApproval(params json.RawMessage) bool {
	var p MemoryEditParams
	if json.Unmarshal(params, &p) == nil {
		return p.Action == "write"
	}
	return true
}

// Execute performs the memory operation.
func (t *MemoryEditTool) Execute(_ context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p MemoryEditParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid memory_edit params: %w", err)
	}

	path := filepath.Join(t.workDir, memoryFileName)

	switch p.Action {
	case "read":
		return t.read(path)
	case "append":
		if strings.TrimSpace(p.Content) == "" {
			return ErrOutput(ErrKindValidation, "content is required for append action"), nil
		}
		return t.append(path, p.Content, p.Section)
	case "write":
		if strings.TrimSpace(p.Content) == "" {
			return ErrOutput(ErrKindValidation, "content is required for write action"), nil
		}
		return t.write(path, p.Content)
	default:
		return ErrOutputf(ErrKindValidation, "invalid action: %s", p.Action), nil
	}
}

func (t *MemoryEditTool) read(path string) (*ToolOutput, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ToolOutput{Content: "(no RYSH.md project memory yet — use action 'append' to create it)"}, nil
		}
		return ErrOutputf(ErrKindInternal, "failed to read memory: %v", err), nil
	}
	if len(data) == 0 {
		return &ToolOutput{Content: "(RYSH.md is empty)"}, nil
	}
	return &ToolOutput{
		Content:  string(data),
		Metadata: map[string]string{"path": path, "size": fmt.Sprintf("%d", len(data))},
	}, nil
}

func (t *MemoryEditTool) append(path, content, section string) (*ToolOutput, error) {
	existing, _ := os.ReadFile(path)
	if len(existing)+len(content) > maxMemorySize {
		return ErrOutputf(ErrKindValidation, "memory would exceed maximum size (%d bytes)", maxMemorySize), nil
	}

	var sb strings.Builder
	if len(existing) == 0 {
		sb.WriteString("# Project Memory\n\n")
		sb.WriteString("_Durable notes auto-loaded into the assistant's context. Edit freely._\n")
	} else {
		sb.Write(existing)
		if !strings.HasSuffix(string(existing), "\n") {
			sb.WriteByte('\n')
		}
	}
	sb.WriteByte('\n')

	if section != "" {
		sb.WriteString(fmt.Sprintf("## %s\n\n", section))
	}
	sb.WriteString(fmt.Sprintf("- %s _(noted %s)_\n", strings.TrimSpace(content), time.Now().Format("2006-01-02")))

	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to write memory: %v", err), nil
	}
	return &ToolOutput{
		Content:  fmt.Sprintf("Recorded to project memory (%s).", path),
		Metadata: map[string]string{"path": path},
	}, nil
}

func (t *MemoryEditTool) write(path, content string) (*ToolOutput, error) {
	if len(content) > maxMemorySize {
		return ErrOutputf(ErrKindValidation, "content exceeds maximum size (%d bytes)", maxMemorySize), nil
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to write memory: %v", err), nil
	}
	return &ToolOutput{
		Content:  fmt.Sprintf("Project memory written (%d chars) to %s.", len(content), path),
		Metadata: map[string]string{"path": path},
	}, nil
}
