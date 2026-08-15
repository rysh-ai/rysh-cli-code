// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const defaultLineLimit = 2000

// FileReadTool reads file contents with optional offset and limit.
type FileReadTool struct {
	workDir string
}

// FileReadParams holds the parameters for reading a file.
type FileReadParams struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset,omitempty"` // line number to start from (0-based)
	Limit    int    `json:"limit,omitempty"`  // max lines to read (default 2000)
}

// NewFileReadTool creates a FileReadTool with the given working directory.
func NewFileReadTool(workDir string) *FileReadTool {
	return &FileReadTool{
		workDir: workDir,
	}
}

// Spec returns the tool specification for the LLM.
func (t *FileReadTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {"type": "string", "description": "Absolute or relative path to the file"},
    "offset": {"type": "integer", "description": "Line number to start reading from (0-based)"},
    "limit": {"type": "integer", "description": "Maximum number of lines to read (default 2000)"}
  },
  "required": ["file_path"]
}`)
	return ToolSpec{
		Name:             "file_read",
		Description:      "Read the contents of a file, optionally starting at an offset and limiting the number of lines returned.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// Execute reads the file described by params.
func (t *FileReadTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p FileReadParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid file_read params: %w", err)
	}

	if p.FilePath == "" {
		return ErrOutput(ErrKindValidation, "file_path is required"), nil
	}

	// Resolve file path: if relative, join with workDir.
	path := p.FilePath
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.workDir, path)
	}

	// Set default limit.
	limit := p.Limit
	if limit <= 0 {
		limit = defaultLineLimit
	}

	// Open file.
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrOutputf(ErrKindMissing, "file not found: %s", path), nil
		}
		return ErrOutputf(ErrKindInternal, "cannot open file: %s", err.Error()), nil
	}
	defer f.Close()

	// Read lines with offset and limit.
	scanner := bufio.NewScanner(f)
	// Increase buffer size to handle long lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var builder strings.Builder
	lineNum := 0
	linesWritten := 0

	for scanner.Scan() {
		if lineNum < p.Offset {
			lineNum++
			continue
		}
		if linesWritten >= limit {
			break
		}
		// Format like "cat -n": right-aligned line number + tab + content.
		displayNum := lineNum + 1 // cat -n uses 1-based line numbers
		builder.WriteString(fmt.Sprintf("%6d\t%s\n", displayNum, scanner.Text()))
		lineNum++
		linesWritten++
	}

	if err := scanner.Err(); err != nil {
		return ErrOutputf(ErrKindInternal, "error reading file: %s", err.Error()), nil
	}

	return &ToolOutput{Content: builder.String()}, nil
}

// RequiresApproval always returns false for read-only operations.
func (t *FileReadTool) RequiresApproval(params json.RawMessage) bool {
	return false
}
