// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const testRunTimeout = 5 * time.Minute

// TestRunTool runs tests and returns structured results.
type TestRunTool struct {
	workDir string
}

// TestRunParams holds the parameters for running tests.
type TestRunParams struct {
	Path    string `json:"path,omitempty"`    // test path/package (e.g., "./...", "./internal/tools/")
	Pattern string `json:"pattern,omitempty"` // test name pattern (-run)
	Verbose bool   `json:"verbose,omitempty"` // verbose output (-v)
	Short   bool   `json:"short,omitempty"`   // run short tests only (-short)
	Count   int    `json:"count,omitempty"`   // run tests N times (-count)
}

// NewTestRunTool creates a TestRunTool with the given working directory.
func NewTestRunTool(workDir string) *TestRunTool {
	return &TestRunTool{workDir: workDir}
}

// Spec returns the tool specification for the LLM.
func (t *TestRunTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Package or path to test (e.g., './...', './internal/tools/'). Default: './...'"},
    "pattern": {"type": "string", "description": "Run only tests matching this regex pattern (passed to -run)"},
    "verbose": {"type": "boolean", "description": "Show verbose test output"},
    "short": {"type": "boolean", "description": "Run only short tests"},
    "count": {"type": "integer", "description": "Run tests N times (useful for flaky test detection)"}
  }
}`)
	return ToolSpec{
		Name:             "test_run",
		Description:      "Run Go tests and return structured results with pass/fail/skip counts and failure details.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval always returns true — tests execute code.
func (t *TestRunTool) RequiresApproval(_ json.RawMessage) bool {
	return true
}

// Execute runs the tests and returns structured output.
func (t *TestRunTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p TestRunParams
	if params != nil {
		_ = json.Unmarshal(params, &p)
	}

	path := p.Path
	if path == "" {
		path = "./..."
	}

	args := []string{"test"}
	if p.Verbose {
		args = append(args, "-v")
	}
	if p.Short {
		args = append(args, "-short")
	}
	if p.Pattern != "" {
		args = append(args, "-run", p.Pattern)
	}
	if p.Count > 0 {
		args = append(args, "-count", fmt.Sprintf("%d", p.Count))
	}
	args = append(args, path)

	execCtx, cancel := context.WithTimeout(ctx, testRunTimeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "go", args...)
	cmd.Dir = t.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{buf: &stdout, limit: maxOutputBytes}
	cmd.Stderr = &limitedWriter{buf: &stderr, limit: maxOutputBytes}

	err := cmd.Run()

	output := stdout.String()
	errOutput := stderr.String()

	// Parse test results.
	passed, failed, skipped := parseTestResults(output)

	// Build structured output.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Test Results: %d passed, %d failed, %d skipped\n", passed, failed, skipped))
	sb.WriteString(strings.Repeat("-", 50))
	sb.WriteByte('\n')

	if err != nil {
		sb.WriteString("STATUS: FAIL\n\n")
	} else {
		sb.WriteString("STATUS: PASS\n\n")
	}

	if output != "" {
		sb.WriteString(output)
	}
	if errOutput != "" {
		sb.WriteString("\nStderr:\n")
		sb.WriteString(errOutput)
	}

	status := "pass"
	if err != nil {
		status = "fail"
	}

	return &ToolOutput{
		Content: sb.String(),
		Metadata: map[string]string{
			"status":  status,
			"passed":  fmt.Sprintf("%d", passed),
			"failed":  fmt.Sprintf("%d", failed),
			"skipped": fmt.Sprintf("%d", skipped),
			"path":    path,
		},
	}, nil
}

var (
	testPassRe = regexp.MustCompile(`--- PASS:`)
	testFailRe = regexp.MustCompile(`--- FAIL:`)
	testSkipRe = regexp.MustCompile(`--- SKIP:`)
)

func parseTestResults(output string) (passed, failed, skipped int) {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if testPassRe.MatchString(line) {
			passed++
		} else if testFailRe.MatchString(line) {
			failed++
		} else if testSkipRe.MatchString(line) {
			skipped++
		}
	}
	return
}
