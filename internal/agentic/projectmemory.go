// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	// projectMemoryFileName is the per-project / per-directory memory file that
	// is auto-loaded into the system prompt, analogous to CLAUDE.md.
	projectMemoryFileName = "RYSH.md"

	// maxProjectMemoryBytes caps the total injected memory so the system prompt
	// stays lean even if memory files grow large.
	maxProjectMemoryBytes = 32 * 1024
)

// LoadProjectMemory gathers durable memory for the given working directory:
//   - the user-global file at ~/.rysh/RYSH.md (lowest precedence), and
//   - every RYSH.md found while walking from the filesystem root down to
//     workDir (so the nearest, most-specific file appears last).
//
// The result is formatted for injection into the system prompt. It returns ""
// when no memory files exist.
func LoadProjectMemory(workDir string) string {
	var sections []string

	// User-global memory (listed first → lowest precedence).
	if home, err := os.UserHomeDir(); err == nil {
		globalPath := filepath.Join(home, ".rysh", projectMemoryFileName)
		if content := readMemoryFile(globalPath); content != "" {
			sections = append(sections, "## Global memory ("+globalPath+")\n\n"+content)
		}
	}

	// Project memory files from filesystem root down to workDir.
	if abs, err := filepath.Abs(workDir); err == nil {
		var dirs []string
		for {
			dirs = append(dirs, abs)
			parent := filepath.Dir(abs)
			if parent == abs {
				break
			}
			abs = parent
		}
		// dirs currently runs cwd→root; iterate in reverse for root→cwd so the
		// nearest directory's memory is appended last.
		for i := len(dirs) - 1; i >= 0; i-- {
			path := filepath.Join(dirs[i], projectMemoryFileName)
			if content := readMemoryFile(path); content != "" {
				sections = append(sections, "## Project memory ("+path+")\n\n"+content)
			}
		}
	}

	if len(sections) == 0 {
		return ""
	}

	joined := strings.Join(sections, "\n\n")
	if len(joined) > maxProjectMemoryBytes {
		joined = joined[:maxProjectMemoryBytes] + "\n\n[...project memory truncated...]"
	}
	return joined
}

func readMemoryFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// appendProjectMemory appends the loaded project/user memory block to a base
// system prompt. It is a no-op when no memory files are present.
func appendProjectMemory(systemPrompt, workDir string) string {
	memory := LoadProjectMemory(workDir)
	if memory == "" {
		return systemPrompt
	}
	return systemPrompt +
		"\n\n# Project & user memory\n" +
		"The following persistent notes were loaded from RYSH.md memory files. " +
		"Treat them as authoritative project conventions and user preferences, " +
		"and use the memory_edit tool to record durable new facts.\n\n" +
		memory
}
