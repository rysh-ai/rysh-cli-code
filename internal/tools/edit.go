// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EditTool performs exact-string replacement(s) in a single existing file.
//
// It supersedes the former file_edit (single replace) and multi_edit (atomic
// multi-hunk) tools: callers pass either a single old_string/new_string pair,
// OR an ordered `edits` array applied atomically (all-or-nothing). The two
// input shapes share one code path. To create or overwrite a whole file use
// file_write instead.
type EditTool struct {
	workDir string
}

// EditPair is a single old→new replacement. (Moved here from the retired
// multi_edit.go so the edits-array form keeps its schema.)
type EditPair struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// EditParams accepts both the single-edit and multi-edit shapes.
type EditParams struct {
	FilePath  string     `json:"file_path"`
	OldString string     `json:"old_string,omitempty"` // single-edit convenience form
	NewString string     `json:"new_string,omitempty"`
	Edits     []EditPair `json:"edits,omitempty"` // multi-edit (atomic) form
}

// NewEditTool creates an EditTool rooted at the given working directory.
func NewEditTool(workDir string) *EditTool { return &EditTool{workDir: workDir} }

// Spec returns the tool specification for the LLM.
func (t *EditTool) Spec() ToolSpec {
	params := json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {"type": "string", "description": "Absolute or relative path to the existing file to edit."},
    "old_string": {"type": "string", "description": "Exact string to find (single-edit form). Must be unique in the file."},
    "new_string": {"type": "string", "description": "Replacement for old_string (single-edit form)."},
    "edits": {
      "type": "array",
      "description": "Ordered find/replace pairs applied atomically (all-or-nothing). Use instead of old_string/new_string for multiple hunks.",
      "items": {
        "type": "object",
        "properties": {
          "old_string": {"type": "string", "description": "Exact string to find; must be unique in the file when matched."},
          "new_string": {"type": "string", "description": "Replacement string."}
        },
        "required": ["old_string", "new_string"]
      }
    }
  },
  "required": ["file_path"]
}`)
	return ToolSpec{
		Name:             "edit",
		Description:      "Edit an existing file by exact string replacement. Provide a single old_string/new_string, or an ordered `edits` array applied atomically (all succeed or none are written). Each old_string must be unique in the file at match time. Returns a unified diff. To create or overwrite a whole file, use file_write.",
		Parameters:       params,
		RequiresApproval: true,
	}
}

// RequiresApproval always returns true — file modifications go through the
// orchestrator's preview-then-approve flow.
func (t *EditTool) RequiresApproval(_ json.RawMessage) bool { return true }

// Execute applies all edits in memory (rolling back on any failure) and writes
// the result, returning a unified diff of the combined change.
func (t *EditTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p EditParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid edit params: %w", err)
	}
	if p.FilePath == "" {
		return ErrOutput(ErrKindValidation, "file_path is required"), nil
	}

	// Normalize to a single edits slice. The single-edit form is prepended as
	// one pair so both input shapes share the loop below.
	edits := p.Edits
	if p.OldString != "" || p.NewString != "" {
		edits = append([]EditPair{{OldString: p.OldString, NewString: p.NewString}}, edits...)
	}
	if len(edits) == 0 {
		return ErrOutput(ErrKindValidation, "provide old_string/new_string or a non-empty edits array"), nil
	}

	filePath := p.FilePath
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(t.workDir, filePath)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		kind := ErrKindInternal
		if os.IsNotExist(err) {
			kind = ErrKindMissing
		}
		return &ToolOutput{
			Error:     fmt.Sprintf("failed to read file: %v", err),
			ErrorKind: kind,
			Metadata:  map[string]string{"file_path": filePath},
		}, nil
	}

	original := string(data)
	content := original

	// Apply each edit sequentially in memory. Any failure returns before the
	// write, preserving multi_edit's atomic (all-or-nothing) semantics.
	for i, e := range edits {
		if e.OldString == "" {
			return &ToolOutput{
				Error:     fmt.Sprintf("edit[%d]: old_string is empty", i),
				ErrorKind: ErrKindValidation,
				Metadata:  map[string]string{"file_path": filePath},
			}, nil
		}
		count := strings.Count(content, e.OldString)
		if count == 0 {
			return &ToolOutput{
				Error:     fmt.Sprintf("edit[%d]: old_string not found in %s (no changes written)", i, filePath),
				ErrorKind: ErrKindValidation,
				Metadata:  map[string]string{"file_path": filePath, "failed_index": fmt.Sprintf("%d", i)},
			}, nil
		}
		if count > 1 {
			return &ToolOutput{
				Error:     fmt.Sprintf("edit[%d]: old_string appears %d times in %s; add context to make it unique (no changes written)", i, count, filePath),
				ErrorKind: ErrKindValidation,
				Metadata:  map[string]string{"file_path": filePath, "failed_index": fmt.Sprintf("%d", i)},
			}, nil
		}
		content = strings.Replace(content, e.OldString, e.NewString, 1)
	}

	diff := generateUnifiedDiff(original, content, filePath)

	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return &ToolOutput{
			Error:     fmt.Sprintf("failed to write file: %v", err),
			ErrorKind: ErrKindInternal,
			Metadata:  map[string]string{"file_path": filePath},
		}, nil
	}

	return &ToolOutput{
		Content:  diff,
		Metadata: map[string]string{"file_path": filePath, "edits_count": fmt.Sprintf("%d", len(edits))},
	}, nil
}

// generateUnifiedDiff produces a unified diff with 3 lines of context. Moved
// here from the retired file_edit.go; file_write.go also depends on it.
func generateUnifiedDiff(oldContent, newContent, filePath string) string {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	// Find the first differing line.
	firstDiff := 0
	minLen := len(oldLines)
	if len(newLines) < minLen {
		minLen = len(newLines)
	}
	for firstDiff < minLen && oldLines[firstDiff] == newLines[firstDiff] {
		firstDiff++
	}

	// Find the last differing line (from the end).
	lastDiffOld := len(oldLines) - 1
	lastDiffNew := len(newLines) - 1
	for lastDiffOld > firstDiff && lastDiffNew > firstDiff &&
		oldLines[lastDiffOld] == newLines[lastDiffNew] {
		lastDiffOld--
		lastDiffNew--
	}

	// Calculate context boundaries.
	contextLines := 3
	startCtx := firstDiff - contextLines
	if startCtx < 0 {
		startCtx = 0
	}
	endCtxOld := lastDiffOld + contextLines
	if endCtxOld >= len(oldLines) {
		endCtxOld = len(oldLines) - 1
	}
	endCtxNew := lastDiffNew + contextLines
	if endCtxNew >= len(newLines) {
		endCtxNew = len(newLines) - 1
	}

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("--- a/%s\n", filePath))
	buf.WriteString(fmt.Sprintf("+++ b/%s\n", filePath))

	oldCount := endCtxOld - startCtx + 1
	newCount := endCtxNew - startCtx + 1
	buf.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", startCtx+1, oldCount, startCtx+1, newCount))

	// Output context before changes.
	for i := startCtx; i < firstDiff; i++ {
		buf.WriteString(" " + oldLines[i] + "\n")
	}
	// Output removed lines.
	for i := firstDiff; i <= lastDiffOld; i++ {
		buf.WriteString("-" + oldLines[i] + "\n")
	}
	// Output added lines.
	for i := firstDiff; i <= lastDiffNew; i++ {
		buf.WriteString("+" + newLines[i] + "\n")
	}
	// Output context after changes.
	for i := lastDiffOld + 1; i <= endCtxOld; i++ {
		buf.WriteString(" " + oldLines[i] + "\n")
	}

	return buf.String()
}
