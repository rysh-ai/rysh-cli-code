// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// GitCommitTool stages files and creates a commit.
type GitCommitTool struct {
	workDir string
}

// GitCommitParams holds the parameters for git commit.
type GitCommitParams struct {
	Message string   `json:"message"`         // commit message
	Files   []string `json:"files,omitempty"` // specific files to stage; empty = stage all tracked changes
	All     bool     `json:"all,omitempty"`   // stage all modified and deleted files (-a)
}

// NewGitCommitTool creates a GitCommitTool with the given working directory.
func NewGitCommitTool(workDir string) *GitCommitTool {
	return &GitCommitTool{workDir: workDir}
}

// Spec returns the tool specification for the LLM.
func (t *GitCommitTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "message": {"type": "string", "description": "The commit message"},
    "files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Specific files to stage before committing. If empty and all=false, commits whatever is already staged."
    },
    "all": {"type": "boolean", "description": "Stage all modified and deleted tracked files before committing (git commit -a)"}
  },
  "required": ["message"]
}`)
	return ToolSpec{
		Name:             "git_commit",
		Description:      "Stage files and create a git commit. Can stage specific files, all tracked changes, or commit the current index.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval always returns true — commits are significant operations.
func (t *GitCommitTool) RequiresApproval(_ json.RawMessage) bool {
	return true
}

// Execute stages files and creates a commit.
func (t *GitCommitTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p GitCommitParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid git_commit params: %w", err)
	}

	if p.Message == "" {
		return ErrOutput(ErrKindValidation, "message is required"), nil
	}

	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Stage files if specified.
	if len(p.Files) > 0 {
		addArgs := append([]string{"add", "--"}, p.Files...)
		addCmd := exec.CommandContext(execCtx, "git", addArgs...)
		addCmd.Dir = t.workDir
		var addErr bytes.Buffer
		addCmd.Stderr = &addErr
		if err := addCmd.Run(); err != nil {
			return ErrOutputf(ErrKindInternal, "git add failed: %s", strings.TrimSpace(addErr.String())), nil
		}
	}

	// Build commit command.
	commitArgs := []string{"commit"}
	if p.All {
		commitArgs = append(commitArgs, "-a")
	}
	commitArgs = append(commitArgs, "-m", p.Message)

	cmd := exec.CommandContext(execCtx, "git", commitArgs...)
	cmd.Dir = t.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return ErrOutputf(ErrKindInternal, "git commit failed: %s", errMsg), nil
	}

	// Get the commit hash.
	hashCmd := exec.CommandContext(execCtx, "git", "rev-parse", "--short", "HEAD")
	hashCmd.Dir = t.workDir
	hashOut, _ := hashCmd.Output()
	hash := strings.TrimSpace(string(hashOut))

	return &ToolOutput{
		Content: stdout.String(),
		Metadata: map[string]string{
			"commit_hash": hash,
			"message":     p.Message,
		},
	}, nil
}
