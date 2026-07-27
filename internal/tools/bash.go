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

const (
	maxOutputBytes = 100 * 1024 // 100KB
	maxTimeout     = 10 * time.Minute
)

// BashTool executes shell commands via bash.
type BashTool struct {
	workDir         string
	timeout         time.Duration
	blockedPatterns []string
}

// BashParams holds the parameters for a bash command execution.
type BashParams struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // ms, overrides default
	WorkDir string `json:"work_dir,omitempty"`
}

// NewBashTool creates a BashTool with the given defaults.
func NewBashTool(workDir string, timeout time.Duration, blockedPatterns []string) *BashTool {
	return &BashTool{
		workDir:         workDir,
		timeout:         timeout,
		blockedPatterns: blockedPatterns,
	}
}

// Spec returns the tool specification for the LLM.
func (t *BashTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "The bash command to execute"},
    "timeout": {"type": "integer", "description": "Timeout in milliseconds (max 600000)"},
    "work_dir": {"type": "string", "description": "Working directory override"}
  },
  "required": ["command"]
}`)
	return ToolSpec{
		Name:             "bash",
		Description:      "Execute a shell command via bash and return its combined stdout/stderr output.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// Execute runs the bash command described by params.
func (t *BashTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p BashParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid bash params: %w", err)
	}

	if p.Command == "" {
		return ErrOutput(ErrKindValidation, "command is required"), nil
	}

	// Determine working directory: param override > tool default.
	workDir := t.workDir
	if p.WorkDir != "" {
		workDir = p.WorkDir
	}

	// Determine timeout: param override > tool default, capped at 10 minutes.
	timeout := t.timeout
	if p.Timeout > 0 {
		timeout = time.Duration(p.Timeout) * time.Millisecond
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	// Create context with timeout.
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "bash", "-c", p.Command)
	cmd.Dir = workDir

	// Follow-up item 5: graceful cancellation. Without this, ctx-cancel
	// defaults to SIGKILL — abrupt and leaves no chance to clean up
	// open sockets / temp files / child processes. Send SIGINT first;
	// after a 3 s grace period, the runtime SIGKILLs.
	gracefulCancel(cmd)

	// Capture combined output, limited to maxOutputBytes.
	var buf bytes.Buffer
	buf.Grow(4096)
	cmd.Stdout = &limitedWriter{buf: &buf, limit: maxOutputBytes}
	cmd.Stderr = cmd.Stdout

	err := cmd.Run()

	output := buf.String()
	exitCode := 0

	if err != nil {
		// Check if the parent context was cancelled (a new user prompt
		// or explicit stop) — surface as a typed `cancelled` error.
		if ctx.Err() != nil {
			result := ErrOutputf(ErrKindCancelled, "command cancelled: %v", ctx.Err())
			result.ExitCode = -1
			return result, nil
		}
		// Check if the context was cancelled due to timeout
		if execCtx.Err() == context.DeadlineExceeded {
			result := ErrOutput(ErrKindTimeout, "command timed out")
			result.Content = output
			result.ExitCode = -1
			return result, nil
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &ToolOutput{
				Content:   output,
				Error:     err.Error(),
				ErrorKind: ErrKindInternal,
				ExitCode:  -1,
			}, nil
		}
	}

	return &ToolOutput{
		Content:  output,
		ExitCode: exitCode,
	}, nil
}

// safeBashPrefixes lists read-only / idempotent command prefixes that never
// require approval. They are matched per-segment against a (possibly chained)
// command. Anything NOT on this list falls through to "require approval" — the
// safe-by-default posture adopted when more operations (git reads, builds,
// directory listings, process inspection) were folded into bash. Extend as
// your environment needs.
var safeBashPrefixes = []string{
	// git read-only subcommands (git_commit and writes stay gated)
	"git status", "git diff", "git log", "git show", "git branch",
	"git remote", "git rev-parse", "git ls-files", "git describe",
	"git blame", "git config --get", "git stash list",
	// directory / file inspection
	"ls", "tree", "pwd", "cat", "head", "tail", "wc", "stat", "file",
	"find", "grep", "rg", "which", "echo", "df", "du", "ps", "lsof",
	// build / test / lint (idempotent for a coding agent)
	"go build", "go test", "go vet", "go list", "go doc", "go env",
	"gofmt", "golangci-lint",
}

// dangerousBashPatterns always require approval regardless of allowlisting.
var dangerousBashPatterns = []string{
	"rm -rf /", "git push --force", "git push -f",
	"git reset --hard", "git clean -f", "> /dev/", "sudo ",
}

// RequiresApproval gates bash with an allowlist. Order of checks:
//  1. user-configured + known-dangerous patterns → always approve;
//  2. redirections / command substitution → approve (can hide mutations);
//  3. each chained segment must be a known-safe prefix, else approve;
//  4. otherwise (all segments read-only/idempotent) → no approval.
//
// This is intentionally safe-by-default: an unrecognised mutating command
// (mkdir, cp, sed -i, make, npm run, …) requires approval. Repeat prompts are
// absorbed by the orchestrator's auto-approval ("yes, always") and any
// configured PermissionPolicy.
func (t *BashTool) RequiresApproval(params json.RawMessage) bool {
	var p BashParams
	if err := json.Unmarshal(params, &p); err != nil {
		return true // if we can't parse, require approval
	}

	cmd := strings.TrimSpace(p.Command)
	if cmd == "" {
		return true
	}

	// 1. User-configured + known-dangerous patterns always gate.
	for _, pattern := range t.blockedPatterns {
		if pattern != "" && strings.Contains(cmd, pattern) {
			return true
		}
	}
	for _, pattern := range dangerousBashPatterns {
		if strings.Contains(cmd, pattern) {
			return true
		}
	}

	// 2. Redirections and command substitution can hide writes/mutations.
	if strings.Contains(cmd, ">") || strings.Contains(cmd, "$(") || strings.Contains(cmd, "`") {
		return true
	}

	// 3. Every segment of a chained command (&& || ; |) must be a safe prefix.
	for _, seg := range splitShellSegments(cmd) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if !hasSafeBashPrefix(seg) {
			return true
		}
	}

	// 4. All segments are read-only/idempotent → no approval.
	return false
}

// splitShellSegments splits a command on shell control operators. strings'
// Replacer matches in argument order, so "&&"/"||" are consumed before "|".
func splitShellSegments(cmd string) []string {
	r := strings.NewReplacer("&&", "\x00", "||", "\x00", ";", "\x00", "|", "\x00", "\n", "\x00")
	return strings.Split(r.Replace(cmd), "\x00")
}

// hasSafeBashPrefix reports whether a single command segment begins with a
// known read-only/idempotent prefix. `find` is treated as safe only when it
// carries no mutating action flag.
func hasSafeBashPrefix(seg string) bool {
	if strings.HasPrefix(seg, "find") &&
		(strings.Contains(seg, " -delete") || strings.Contains(seg, " -exec") || strings.Contains(seg, " -ok")) {
		return false
	}
	for _, pre := range safeBashPrefixes {
		if seg == pre || strings.HasPrefix(seg, pre+" ") {
			return true
		}
	}
	return false
}

// limitedWriter wraps a buffer and stops writing after limit bytes.
type limitedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		return len(p), nil // discard but report success
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	return w.buf.Write(p)
}
