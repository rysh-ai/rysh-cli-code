// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const grepOutputLimit = 100 * 1024 // ~100KB

// GrepTool searches file contents using regular expressions.
type GrepTool struct {
	workDir string
}

// GrepParams holds the parameters for grep searching.
type GrepParams struct {
	Pattern    string `json:"pattern"`               // regex pattern
	Path       string `json:"path,omitempty"`        // file or directory to search
	Glob       string `json:"glob,omitempty"`        // filter files by glob (e.g., "*.go")
	OutputMode string `json:"output_mode,omitempty"` // "content", "files_with_matches", "count"
	Context    int    `json:"context,omitempty"`     // lines of context around matches
}

// NewGrepTool creates a GrepTool with the given working directory.
func NewGrepTool(workDir string) *GrepTool {
	return &GrepTool{
		workDir: workDir,
	}
}

// Spec returns the tool specification for the LLM.
func (t *GrepTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Regular expression pattern to search for"},
    "path": {"type": "string", "description": "File or directory to search in. Defaults to working directory."},
    "glob": {"type": "string", "description": "Glob pattern to filter files (e.g., \"*.go\", \"*.{ts,tsx}\")"},
    "output_mode": {"type": "string", "enum": ["content", "files_with_matches", "count"], "description": "Output mode: content (matching lines), files_with_matches (file paths), count (match counts). Default: files_with_matches"},
    "context": {"type": "integer", "description": "Number of lines of context to show around each match (only for content mode)"}
  },
  "required": ["pattern"]
}`)
	return ToolSpec{
		Name:             "grep",
		Description:      "Search file contents using regular expressions. Supports filtering by file glob pattern.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// Execute performs the grep search described by params.
func (t *GrepTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p GrepParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid grep params: %w", err)
	}

	if p.Pattern == "" {
		return ErrOutput(ErrKindValidation, "pattern is required"), nil
	}

	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return ErrOutputf(ErrKindValidation, "invalid regex pattern: %s", err.Error()), nil
	}

	// Determine search path.
	searchPath := t.workDir
	if p.Path != "" {
		if filepath.IsAbs(p.Path) {
			searchPath = p.Path
		} else {
			searchPath = filepath.Join(t.workDir, p.Path)
		}
	}

	// Default output mode.
	outputMode := p.OutputMode
	if outputMode == "" {
		outputMode = "files_with_matches"
	}

	// Check if searchPath is a file or directory.
	info, err := os.Stat(searchPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrOutputf(ErrKindMissing, "path not found: %s", searchPath), nil
		}
		return ErrOutputf(ErrKindInternal, "cannot access path: %s", err.Error()), nil
	}

	var results grepResults
	results.outputMode = outputMode
	results.contextLines = p.Context

	if info.IsDir() {
		err = filepath.WalkDir(searchPath, func(path string, d fs.DirEntry, walkErr error) error {
			if cerr := ctx.Err(); cerr != nil {
				return cerr // abort the walk on cancellation/timeout (follow-up 5b)
			}
			if walkErr != nil {
				return nil // skip inaccessible
			}
			if d.IsDir() {
				// Skip hidden directories.
				if strings.HasPrefix(d.Name(), ".") && path != searchPath {
					return filepath.SkipDir
				}
				return nil
			}

			// Apply glob filter.
			if p.Glob != "" {
				matched, _ := filepath.Match(p.Glob, d.Name())
				if !matched {
					return nil
				}
			}

			// Get relative path for display.
			rel, relErr := filepath.Rel(searchPath, path)
			if relErr != nil {
				rel = path
			}

			if results.outputSize >= grepOutputLimit {
				return fs.SkipAll
			}

			searchFile(ctx, path, rel, re, &results)
			return nil
		})
	} else {
		rel, relErr := filepath.Rel(t.workDir, searchPath)
		if relErr != nil {
			rel = searchPath
		}
		searchFile(ctx, searchPath, rel, re, &results)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ErrOutputf(ErrKindCancelled, "search cancelled: %v", err), nil
		}
		return ErrOutputf(ErrKindInternal, "search error: %s", err.Error()), nil
	}

	content := results.format()
	return &ToolOutput{Content: content}, nil
}

// RequiresApproval always returns false for read-only operations.
func (t *GrepTool) RequiresApproval(params json.RawMessage) bool {
	return false
}

// grepResults accumulates search results.
type grepResults struct {
	outputMode   string
	contextLines int
	outputSize   int

	// For files_with_matches mode.
	matchedFiles []string

	// For count mode.
	fileCounts []fileCount

	// For content mode.
	contentLines []string
}

type fileCount struct {
	path  string
	count int
}

func (r *grepResults) format() string {
	switch r.outputMode {
	case "files_with_matches":
		sort.Strings(r.matchedFiles)
		return strings.Join(r.matchedFiles, "\n")
	case "count":
		sort.Slice(r.fileCounts, func(i, j int) bool {
			return r.fileCounts[i].path < r.fileCounts[j].path
		})
		var b strings.Builder
		for _, fc := range r.fileCounts {
			b.WriteString(fmt.Sprintf("%s:%d\n", fc.path, fc.count))
		}
		return b.String()
	case "content":
		return strings.Join(r.contentLines, "\n")
	default:
		return strings.Join(r.matchedFiles, "\n")
	}
}

// searchFile searches a single file for regex matches.
func searchFile(ctx context.Context, absPath, relPath string, re *regexp.Regexp, results *grepResults) {
	// Check context cancellation.
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Skip binary files by checking the first 512 bytes for null bytes.
	if isBinary(absPath) {
		return
	}

	f, err := os.Open(absPath)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	switch results.outputMode {
	case "files_with_matches":
		for scanner.Scan() {
			if re.MatchString(scanner.Text()) {
				results.matchedFiles = append(results.matchedFiles, relPath)
				results.outputSize += len(relPath) + 1
				return
			}
		}

	case "count":
		count := 0
		for scanner.Scan() {
			if re.MatchString(scanner.Text()) {
				count++
			}
		}
		if count > 0 {
			results.fileCounts = append(results.fileCounts, fileCount{path: relPath, count: count})
			results.outputSize += len(relPath) + 10
		}

	case "content":
		// Read all lines for context support.
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}

		matchIndices := make(map[int]bool)
		for i, line := range lines {
			if re.MatchString(line) {
				matchIndices[i] = true
			}
		}

		if len(matchIndices) == 0 {
			return
		}

		// Determine which lines to show (matches + context).
		showLines := make(map[int]bool)
		for idx := range matchIndices {
			for c := idx - results.contextLines; c <= idx+results.contextLines; c++ {
				if c >= 0 && c < len(lines) {
					showLines[c] = true
				}
			}
		}

		// Build sorted list of line indices to show.
		sortedIndices := make([]int, 0, len(showLines))
		for idx := range showLines {
			sortedIndices = append(sortedIndices, idx)
		}
		sort.Ints(sortedIndices)

		// Output with file header.
		header := fmt.Sprintf("--- %s ---", relPath)
		results.contentLines = append(results.contentLines, header)
		results.outputSize += len(header) + 1

		prevIdx := -2
		for _, idx := range sortedIndices {
			if results.outputSize >= grepOutputLimit {
				return
			}

			// Insert separator for non-contiguous lines.
			if prevIdx >= 0 && idx > prevIdx+1 {
				results.contentLines = append(results.contentLines, "--")
				results.outputSize += 3
			}

			lineNum := idx + 1
			line := fmt.Sprintf("%d:%s", lineNum, lines[idx])
			results.contentLines = append(results.contentLines, line)
			results.outputSize += len(line) + 1
			prevIdx = idx
		}

		results.contentLines = append(results.contentLines, "")
		results.outputSize++
	}
}

// isBinary checks if a file appears to be binary by looking for null bytes
// in the first 512 bytes.
func isBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	if n == 0 {
		return false
	}

	for i := 0; i < n; i++ {
		if buf[i] == 0 {
			return true
		}
	}
	return false
}
