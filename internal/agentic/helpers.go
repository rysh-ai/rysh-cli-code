package agentic

import (
	"strings"
	"time"
)

// formatTimestamp returns a compact timestamp for logging.
func formatTimestamp() string {
	return time.Now().Format("15:04:05")
}

// truncate shortens a string to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// indentBlock indents every line of text by prefix.
func indentBlock(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
