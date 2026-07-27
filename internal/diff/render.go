package diff

import (
	"fmt"
	"strings"
)

const (
	ansiReset = "\033[0m"
	ansiBold  = "\033[1m"
	ansiRed   = "\033[31m"
	ansiGreen = "\033[32m"
	ansiCyan  = "\033[36m"
)

// Render takes a unified diff string and returns it with ANSI color codes.
// - "---" and "+++" header lines: bold
// - "@@" hunk headers: cyan
// - "-" removed lines: red
// - "+" added lines: green
// - context lines: default (no color)
func Render(unifiedDiff string) string {
	if unifiedDiff == "" {
		return ""
	}

	lines := strings.Split(unifiedDiff, "\n")
	var sb strings.Builder

	for i, line := range lines {
		colored := colorLine(line)
		sb.WriteString(colored)
		if i < len(lines)-1 {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// RenderCompact is like Render but truncates output to maxLines lines.
// If the diff exceeds maxLines, a "... (N more lines)" message is appended.
func RenderCompact(unifiedDiff string, maxLines int) string {
	if unifiedDiff == "" {
		return ""
	}

	lines := strings.Split(unifiedDiff, "\n")
	// Remove trailing empty line from split if present
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) <= maxLines {
		return Render(unifiedDiff)
	}

	var sb strings.Builder
	for i := 0; i < maxLines; i++ {
		sb.WriteString(colorLine(lines[i]))
		sb.WriteString("\n")
	}

	remaining := len(lines) - maxLines
	sb.WriteString(fmt.Sprintf("... (%d more lines)\n", remaining))

	return sb.String()
}

// colorLine applies ANSI color codes to a single diff line.
func colorLine(line string) string {
	if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
		return ansiBold + line + ansiReset
	}
	if strings.HasPrefix(line, "@@") {
		return ansiCyan + line + ansiReset
	}
	if strings.HasPrefix(line, "-") {
		return ansiRed + line + ansiReset
	}
	if strings.HasPrefix(line, "+") {
		return ansiGreen + line + ansiReset
	}
	return line
}
