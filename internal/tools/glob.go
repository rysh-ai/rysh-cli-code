// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

const globResultLimit = 1000

// GlobTool finds files matching a glob pattern.
type GlobTool struct {
	workDir string
}

// GlobParams holds the parameters for glob matching.
type GlobParams struct {
	Pattern string `json:"pattern"`        // e.g., "**/*.go", "src/**/*.ts"
	Path    string `json:"path,omitempty"` // base directory, defaults to workDir
}

// NewGlobTool creates a GlobTool with the given working directory.
func NewGlobTool(workDir string) *GlobTool {
	return &GlobTool{
		workDir: workDir,
	}
}

// Spec returns the tool specification for the LLM.
func (t *GlobTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Glob pattern to match files (e.g., \"**/*.go\", \"src/**/*.ts\")"},
    "path": {"type": "string", "description": "Base directory to search in. Defaults to working directory."}
  },
  "required": ["pattern"]
}`)
	return ToolSpec{
		Name:             "glob",
		Description:      "Find files matching a glob pattern. Supports ** for recursive matching.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// Execute performs the glob matching described by params.
func (t *GlobTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p GlobParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid glob params: %w", err)
	}

	if p.Pattern == "" {
		return ErrOutput(ErrKindValidation, "pattern is required"), nil
	}

	// Determine base path.
	basePath := t.workDir
	if p.Path != "" {
		if filepath.IsAbs(p.Path) {
			basePath = p.Path
		} else {
			basePath = filepath.Join(t.workDir, p.Path)
		}
	}

	var matches []string
	var err error

	if strings.Contains(p.Pattern, "**") {
		matches, err = recursiveGlob(ctx, basePath, p.Pattern)
	} else {
		matches, err = simpleGlob(basePath, p.Pattern)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ErrOutputf(ErrKindCancelled, "glob cancelled: %v", err), nil
		}
		return ErrOutputf(ErrKindInternal, "glob error: %s", err.Error()), nil
	}

	sort.Strings(matches)

	// Truncate results if over limit.
	truncated := false
	total := len(matches)
	if total > globResultLimit {
		matches = matches[:globResultLimit]
		truncated = true
	}

	var builder strings.Builder
	for _, m := range matches {
		builder.WriteString(m)
		builder.WriteByte('\n')
	}
	if truncated {
		builder.WriteString(fmt.Sprintf("... (%d more)\n", total-globResultLimit))
	}

	return &ToolOutput{Content: builder.String()}, nil
}

// RequiresApproval always returns false for read-only operations.
func (t *GlobTool) RequiresApproval(params json.RawMessage) bool {
	return false
}

// simpleGlob handles patterns without ** by using filepath.Glob directly.
func simpleGlob(basePath, pattern string) ([]string, error) {
	fullPattern := filepath.Join(basePath, pattern)
	absMatches, err := filepath.Glob(fullPattern)
	if err != nil {
		return nil, err
	}

	// Convert to relative paths.
	matches := make([]string, 0, len(absMatches))
	for _, m := range absMatches {
		rel, err := filepath.Rel(basePath, m)
		if err != nil {
			rel = m
		}
		matches = append(matches, rel)
	}
	return matches, nil
}

// recursiveGlob handles patterns containing ** by walking the directory tree.
// ctx is honoured so a superseded/cancelled prompt aborts a large-tree walk
// promptly (follow-up 5b).
func recursiveGlob(ctx context.Context, basePath, pattern string) ([]string, error) {
	var matches []string

	err := filepath.WalkDir(basePath, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr // abort the walk on cancellation/timeout
		}
		if err != nil {
			return nil // skip inaccessible paths
		}

		rel, err := filepath.Rel(basePath, path)
		if err != nil {
			return nil
		}

		// Skip the base directory itself.
		if rel == "." {
			return nil
		}

		if matchGlob(pattern, rel) {
			matches = append(matches, rel)
		}
		return nil
	})

	return matches, err
}

// matchGlob checks whether a path matches a pattern that may contain **.
// The ** wildcard matches zero or more directory levels.
func matchGlob(pattern, path string) bool {
	// Normalize separators.
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)

	return doMatchGlob(pattern, path)
}

// doMatchGlob is the recursive implementation of glob matching with ** support.
func doMatchGlob(pattern, path string) bool {
	// If pattern has no **, use filepath.Match.
	if !strings.Contains(pattern, "**") {
		matched, _ := filepath.Match(pattern, path)
		return matched
	}

	// Split pattern at the first **.
	idx := strings.Index(pattern, "**")
	prefix := pattern[:idx]
	suffix := pattern[idx+2:]

	// Remove leading separator from suffix if present.
	suffix = strings.TrimPrefix(suffix, "/")

	// The prefix must match the beginning of the path.
	if prefix != "" {
		// prefix ends with "/" (e.g., "src/"), check that path starts with it.
		if !strings.HasPrefix(path, prefix) {
			// Try filepath.Match on prefix segments.
			prefixParts := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
			pathParts := strings.Split(path, "/")
			if len(pathParts) < len(prefixParts) {
				return false
			}
			for i, pp := range prefixParts {
				matched, _ := filepath.Match(pp, pathParts[i])
				if !matched {
					return false
				}
			}
			// Trim matched prefix from path.
			path = strings.Join(pathParts[len(prefixParts):], "/")
		} else {
			path = path[len(prefix):]
		}
	}

	// If no suffix remains, ** matches everything.
	if suffix == "" {
		return true
	}

	// Try matching suffix against every possible tail of the path.
	// ** can match zero or more path segments.
	pathParts := strings.Split(path, "/")
	for i := 0; i <= len(pathParts); i++ {
		remainder := strings.Join(pathParts[i:], "/")
		if doMatchGlob(suffix, remainder) {
			return true
		}
	}

	return false
}
