// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	monitorDefaultDuration = 10
	monitorMaxDuration     = 60
)

// MonitorTool runs a background command and returns output lines.
type MonitorTool struct {
	workDir string
}

// MonitorParams holds the parameters for the monitor tool.
type MonitorParams struct {
	Command  string `json:"command"`
	Duration int    `json:"duration,omitempty"` // seconds, default 10, max 60
	Pattern  string `json:"pattern,omitempty"`  // regex to filter output lines
}

// NewMonitorTool creates a MonitorTool with the given working directory.
func NewMonitorTool(workDir string) *MonitorTool {
	return &MonitorTool{workDir: workDir}
}

// Spec returns the tool specification for the LLM.
func (t *MonitorTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "The command to run and monitor output from"},
    "duration": {"type": "integer", "description": "How long to run the command in seconds (default 10, max 60)"},
    "pattern": {"type": "string", "description": "Regular expression to filter output lines. Only matching lines are returned."}
  },
  "required": ["command"]
}`)
	return ToolSpec{
		Name:             "monitor",
		Description:      "Run a command for a specified duration and collect its output. Optionally filter output lines by regex pattern.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval always returns true since this tool executes commands.
func (t *MonitorTool) RequiresApproval(_ json.RawMessage) bool {
	return true
}

// Execute runs the command and collects output for the specified duration.
func (t *MonitorTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p MonitorParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}

	if p.Command == "" {
		return ErrOutput(ErrKindValidation, "command is required"), nil
	}

	duration := p.Duration
	if duration <= 0 {
		duration = monitorDefaultDuration
	}
	if duration > monitorMaxDuration {
		duration = monitorMaxDuration
	}

	// Compile pattern filter if provided
	var filter *regexp.Regexp
	if p.Pattern != "" {
		var err error
		filter, err = regexp.Compile(p.Pattern)
		if err != nil {
			return ErrOutputf(ErrKindValidation, "invalid regex pattern: %v", err), nil
		}
	}

	// Create command with timeout context
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(duration)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "/bin/sh", "-c", p.Command)
	cmd.Dir = t.workDir

	// Combine stdout and stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to create stdout pipe: %v", err), nil
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout

	if err := cmd.Start(); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to start command: %v", err), nil
	}

	// Read output lines
	var allLines []string
	var matchedLines []string
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		allLines = append(allLines, line)

		if filter != nil {
			if filter.MatchString(line) {
				matchedLines = append(matchedLines, line)
			}
		} else {
			matchedLines = append(matchedLines, line)
		}
	}

	// Wait for command to finish (may have been killed by context timeout)
	_ = cmd.Wait()

	linesTotal := len(allLines)
	linesMatched := len(matchedLines)

	// Build output
	var sb strings.Builder
	if len(matchedLines) == 0 {
		sb.WriteString("(no output")
		if filter != nil {
			sb.WriteString(" matching pattern")
		}
		sb.WriteString(")\n")
	} else {
		sb.WriteString(strings.Join(matchedLines, "\n"))
		sb.WriteString("\n")
	}

	return &ToolOutput{
		Content: sb.String(),
		Metadata: map[string]string{
			"lines_total":   strconv.Itoa(linesTotal),
			"lines_matched": strconv.Itoa(linesMatched),
			"duration":      strconv.Itoa(duration) + "s",
			"command":       p.Command,
		},
	}, nil
}
