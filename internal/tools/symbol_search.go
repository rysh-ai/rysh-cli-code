package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const symbolResultLimit = 200

// SymbolSearchTool finds function, type, and variable declarations by name.
type SymbolSearchTool struct {
	workDir string
}

// SymbolSearchParams holds the parameters for symbol search.
type SymbolSearchParams struct {
	Query    string `json:"query"`              // symbol name or pattern to search for
	Kind     string `json:"kind,omitempty"`     // "function", "type", "const", "var", "all" (default: "all")
	Language string `json:"language,omitempty"` // "go", "python", "javascript", "typescript", "rust" (auto-detect if empty)
	Path     string `json:"path,omitempty"`     // restrict search to this path
}

// NewSymbolSearchTool creates a SymbolSearchTool with the given working directory.
func NewSymbolSearchTool(workDir string) *SymbolSearchTool {
	return &SymbolSearchTool{workDir: workDir}
}

// Spec returns the tool specification for the LLM.
func (t *SymbolSearchTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Symbol name or regex pattern to search for (e.g., 'NewServer', 'Handle.*Request')"},
    "kind": {"type": "string", "enum": ["function", "type", "const", "var", "all"], "description": "Kind of symbol to search for (default: all)"},
    "language": {"type": "string", "enum": ["go", "python", "javascript", "typescript", "rust"], "description": "Programming language (auto-detects if omitted)"},
    "path": {"type": "string", "description": "Restrict search to this file or directory"}
  },
  "required": ["query"]
}`)
	return ToolSpec{
		Name:             "symbol_search",
		Description:      "Find function, type, constant, and variable declarations by name across the codebase. More precise than grep for code navigation.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false — read-only operation.
func (t *SymbolSearchTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute searches for symbols matching the query.
func (t *SymbolSearchTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p SymbolSearchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid symbol_search params: %w", err)
	}

	if p.Query == "" {
		return ErrOutput(ErrKindValidation, "query is required"), nil
	}

	queryRe, err := regexp.Compile(p.Query)
	if err != nil {
		return ErrOutputf(ErrKindValidation, "invalid query pattern: %s", err), nil
	}

	searchPath := t.workDir
	if p.Path != "" {
		if filepath.IsAbs(p.Path) {
			searchPath = p.Path
		} else {
			searchPath = filepath.Join(t.workDir, p.Path)
		}
	}

	kind := p.Kind
	if kind == "" {
		kind = "all"
	}

	var results []symbolResult

	filepath.WalkDir(searchPath, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != searchPath {
				return filepath.SkipDir
			}
			if d.Name() == "vendor" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return fs.SkipAll
		default:
		}

		lang := p.Language
		if lang == "" {
			lang = detectLanguage(d.Name())
		}
		if lang == "" {
			return nil
		}

		patterns := getSymbolPatterns(lang, kind)
		if len(patterns) == 0 {
			return nil
		}

		rel, _ := filepath.Rel(t.workDir, path)
		if rel == "" {
			rel = path
		}

		fileResults := searchFileSymbols(path, rel, queryRe, patterns)
		results = append(results, fileResults...)

		if len(results) >= symbolResultLimit {
			return fs.SkipAll
		}
		return nil
	})

	if len(results) == 0 {
		return &ToolOutput{Content: fmt.Sprintf("No symbols matching '%s' found.", p.Query)}, nil
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].file != results[j].file {
			return results[i].file < results[j].file
		}
		return results[i].line < results[j].line
	})

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d symbol(s) matching '%s':\n\n", len(results), p.Query))
	for _, r := range results {
		sb.WriteString(fmt.Sprintf("  %s:%d  [%s]  %s\n", r.file, r.line, r.kind, r.text))
	}

	return &ToolOutput{Content: sb.String()}, nil
}

type symbolResult struct {
	file string
	line int
	kind string
	text string
}

type symbolPattern struct {
	kind    string
	pattern *regexp.Regexp
}

func detectLanguage(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".jsx", ".mjs":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".rs":
		return "rust"
	default:
		return ""
	}
}

// Pre-compiled symbol-detection regexes. Compiled once at package init rather
// than recompiled on every symbol_search query.
var (
	reSymGoFunc    = regexp.MustCompile(`^func\s+(\([^)]*\)\s+)?(\w+)`)
	reSymGoType    = regexp.MustCompile(`^type\s+(\w+)`)
	reSymGoConst   = regexp.MustCompile(`^\s*(\w+)\s*=|^const\s+(\w+)`)
	reSymGoVar     = regexp.MustCompile(`^var\s+(\w+)`)
	reSymPyFunc    = regexp.MustCompile(`^(async\s+)?def\s+(\w+)`)
	reSymPyClass   = regexp.MustCompile(`^class\s+(\w+)`)
	reSymPyVar     = regexp.MustCompile(`^(\w+)\s*=`)
	reSymJSFunc    = regexp.MustCompile(`^(export\s+)?(async\s+)?function\s+(\w+)`)
	reSymJSConstFn = regexp.MustCompile(`^(export\s+)?const\s+(\w+)\s*=\s*(async\s+)?\(`)
	reSymJSType    = regexp.MustCompile(`^(export\s+)?(class|interface|type|enum)\s+(\w+)`)
	reSymJSConst   = regexp.MustCompile(`^(export\s+)?const\s+(\w+)`)
	reSymJSVar     = regexp.MustCompile(`^(export\s+)?(let|var)\s+(\w+)`)
	reSymRsFn      = regexp.MustCompile(`^(pub\s+)?(async\s+)?fn\s+(\w+)`)
	reSymRsType    = regexp.MustCompile(`^(pub\s+)?(struct|enum|trait|type)\s+(\w+)`)
	reSymRsConst   = regexp.MustCompile(`^(pub\s+)?const\s+(\w+)`)
)

func getSymbolPatterns(lang, kind string) []symbolPattern {
	var patterns []symbolPattern

	switch lang {
	case "go":
		if kind == "all" || kind == "function" {
			patterns = append(patterns, symbolPattern{"func", reSymGoFunc})
		}
		if kind == "all" || kind == "type" {
			patterns = append(patterns, symbolPattern{"type", reSymGoType})
		}
		if kind == "all" || kind == "const" {
			patterns = append(patterns, symbolPattern{"const", reSymGoConst})
		}
		if kind == "all" || kind == "var" {
			patterns = append(patterns, symbolPattern{"var", reSymGoVar})
		}

	case "python":
		if kind == "all" || kind == "function" {
			patterns = append(patterns, symbolPattern{"func", reSymPyFunc})
		}
		if kind == "all" || kind == "type" {
			patterns = append(patterns, symbolPattern{"class", reSymPyClass})
		}
		if kind == "all" || kind == "var" {
			patterns = append(patterns, symbolPattern{"var", reSymPyVar})
		}

	case "javascript", "typescript":
		if kind == "all" || kind == "function" {
			patterns = append(patterns, symbolPattern{"func", reSymJSFunc})
			patterns = append(patterns, symbolPattern{"func", reSymJSConstFn})
		}
		if kind == "all" || kind == "type" {
			patterns = append(patterns, symbolPattern{"type", reSymJSType})
		}
		if kind == "all" || kind == "const" {
			patterns = append(patterns, symbolPattern{"const", reSymJSConst})
		}
		if kind == "all" || kind == "var" {
			patterns = append(patterns, symbolPattern{"var", reSymJSVar})
		}

	case "rust":
		if kind == "all" || kind == "function" {
			patterns = append(patterns, symbolPattern{"fn", reSymRsFn})
		}
		if kind == "all" || kind == "type" {
			patterns = append(patterns, symbolPattern{"type", reSymRsType})
		}
		if kind == "all" || kind == "const" {
			patterns = append(patterns, symbolPattern{"const", reSymRsConst})
		}
	}

	return patterns
}

func searchFileSymbols(absPath, relPath string, queryRe *regexp.Regexp, patterns []symbolPattern) []symbolResult {
	if isBinary(absPath) {
		return nil
	}

	f, err := os.Open(absPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var results []symbolResult
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		for _, sp := range patterns {
			matches := sp.pattern.FindStringSubmatch(line)
			if matches == nil {
				continue
			}
			// Find the symbol name in the match groups (last non-empty group).
			symbolName := ""
			for i := len(matches) - 1; i >= 1; i-- {
				if matches[i] != "" {
					symbolName = matches[i]
					break
				}
			}
			if symbolName == "" {
				continue
			}
			if queryRe.MatchString(symbolName) {
				results = append(results, symbolResult{
					file: relPath,
					line: lineNum,
					kind: sp.kind,
					text: strings.TrimSpace(line),
				})
			}
		}
	}

	return results
}
