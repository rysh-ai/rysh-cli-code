package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileWriteTool creates or overwrites files.
type FileWriteTool struct {
	workDir string
}

// FileWriteParams holds the parameters for a file write operation.
type FileWriteParams struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// NewFileWriteTool creates a FileWriteTool rooted at the given working directory.
func NewFileWriteTool(workDir string) *FileWriteTool {
	return &FileWriteTool{workDir: workDir}
}

// Spec returns the tool specification for file_write.
func (t *FileWriteTool) Spec() ToolSpec {
	params := json.RawMessage(`{
	"type": "object",
	"properties": {
		"file_path": {
			"type": "string",
			"description": "Absolute or relative path to the file to create or overwrite."
		},
		"content": {
			"type": "string",
			"description": "The full content to write to the file."
		}
	},
	"required": ["file_path", "content"]
}`)
	return ToolSpec{
		Name:             "file_write",
		Description:      "Create a new file or overwrite an existing file with the provided content.",
		Parameters:       params,
		RequiresApproval: true,
	}
}

// RequiresApproval always returns true because file modifications need approval.
func (t *FileWriteTool) RequiresApproval(params json.RawMessage) bool {
	return true
}

// Execute writes the content to the specified file and returns a unified diff.
func (t *FileWriteTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p FileWriteParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	// Resolve file path.
	filePath := p.FilePath
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(t.workDir, filePath)
	}

	// Check if file exists and read old content for diff.
	var oldContent string
	var operation string
	if data, err := os.ReadFile(filePath); err == nil {
		oldContent = string(data)
		operation = "overwrite"
	} else {
		operation = "create"
	}

	// Generate diff.
	var diff string
	if operation == "overwrite" {
		diff = generateWriteDiff(oldContent, p.Content, filePath)
	} else {
		diff = generateNewFileDiff(p.Content, filePath)
	}

	// Create parent directories if needed.
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &ToolOutput{
			Content:   "",
			Error:     fmt.Sprintf("failed to create directories: %s", err),
			ErrorKind: ErrKindInternal,
			Metadata: map[string]string{
				"file_path": filePath,
			},
		}, nil
	}

	// Write file.
	if err := os.WriteFile(filePath, []byte(p.Content), 0644); err != nil {
		return &ToolOutput{
			Content:   "",
			Error:     fmt.Sprintf("failed to write file: %s", err),
			ErrorKind: ErrKindInternal,
			Metadata: map[string]string{
				"file_path": filePath,
			},
		}, nil
	}

	return &ToolOutput{
		Content: diff,
		Metadata: map[string]string{
			"file_path": filePath,
			"operation": operation,
		},
	}, nil
}

// generateWriteDiff produces a unified diff for overwriting an existing file.
func generateWriteDiff(oldContent, newContent, filePath string) string {
	return generateUnifiedDiff(oldContent, newContent, filePath)
}

// generateNewFileDiff produces an all-added diff for a newly created file.
func generateNewFileDiff(content, filePath string) string {
	lines := strings.Split(content, "\n")

	var buf strings.Builder
	buf.WriteString("--- /dev/null\n")
	buf.WriteString(fmt.Sprintf("+++ b/%s\n", filePath))
	buf.WriteString(fmt.Sprintf("@@ -0,0 +1,%d @@\n", len(lines)))

	for _, line := range lines {
		buf.WriteString("+" + line + "\n")
	}

	return buf.String()
}
