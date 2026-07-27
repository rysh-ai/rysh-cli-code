// Package eval implements the agent eval harness (design 009): "unit tests for
// agents." A fixture describes a task and structural assertions; a Result is
// what running the agent produced; Evaluate checks the Result against the
// assertions. The format is open and in-repo.
//
// This package is the fixture format + the pure, deterministic assertion engine
// (no token spend). Live execution that PRODUCES a Result by running the agent
// (`rysh run`) is the integration half; the structural mode here runs against a
// recorded/produced Result artifact.
package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Expect is the assertion set for a case (evals/<case>/expect.yaml).
type Expect struct {
	// FilesChanged: every changed file must match at least one of these globs
	// (scope containment — catches out-of-scope edits). Empty ⇒ not checked.
	FilesChanged []string `yaml:"files_changed"`
	// CommandsAllowed: if non-empty, every executed command must match one.
	CommandsAllowed []string `yaml:"commands_allowed"`
	// CommandsForbidden: no executed command may match any of these.
	CommandsForbidden []string `yaml:"commands_forbidden"`
	// OutputMatches: every regex must match somewhere in the output.
	OutputMatches []string `yaml:"output_matches"`
	// MaxTokens: tokens_used must be ≤ this (0 ⇒ not checked).
	MaxTokens int `yaml:"max_tokens"`
}

// Result is what an agent run produced (structural facts, not a transcript).
type Result struct {
	FilesChanged []string `json:"files_changed"`
	Commands     []string `json:"commands"`
	Output       string   `json:"output"`
	TokensUsed   int      `json:"tokens_used"`
}

// Case is a loaded fixture.
type Case struct {
	Name   string
	Prompt string
	Until  string
	Expect Expect
}

// Assertion is one checked rule.
type Assertion struct {
	Name   string
	Pass   bool
	Detail string
}

// taskFrontmatter is the optional YAML block at the top of task.md.
type taskFrontmatter struct {
	Until string `yaml:"until"`
}

// LoadCase loads evals/<case>/task.md (+ optional frontmatter) and expect.yaml.
func LoadCase(dir string) (*Case, error) {
	taskData, err := os.ReadFile(filepath.Join(dir, "task.md"))
	if err != nil {
		return nil, fmt.Errorf("read task.md: %w", err)
	}
	fm, body := splitFrontmatter(taskData)
	c := &Case{Name: filepath.Base(dir), Prompt: strings.TrimSpace(body)}
	if fm != "" {
		var t taskFrontmatter
		if err := yaml.Unmarshal([]byte(fm), &t); err == nil {
			c.Until = t.Until
		}
	}
	expData, err := os.ReadFile(filepath.Join(dir, "expect.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read expect.yaml: %w", err)
	}
	if err := yaml.Unmarshal(expData, &c.Expect); err != nil {
		return nil, fmt.Errorf("parse expect.yaml: %w", err)
	}
	return c, nil
}

// splitFrontmatter separates a leading --- YAML block from the body.
func splitFrontmatter(data []byte) (fm, body string) {
	s := string(data)
	if !strings.HasPrefix(s, "---") {
		return "", s
	}
	rest := strings.TrimPrefix(s, "---")
	if i := strings.Index(rest, "\n---"); i >= 0 {
		return strings.TrimSpace(rest[:i]), strings.TrimLeft(rest[i+4:], "\n")
	}
	return "", s
}

// Evaluate checks a Result against an Expect, returning one Assertion per rule.
func Evaluate(e Expect, r Result) []Assertion {
	var out []Assertion

	if len(e.FilesChanged) > 0 {
		var offenders []string
		for _, f := range r.FilesChanged {
			if !matchesAnyPath(e.FilesChanged, f) {
				offenders = append(offenders, f)
			}
		}
		out = append(out, assert("files_changed in scope", len(offenders) == 0,
			fmt.Sprintf("out-of-scope: %v", offenders)))
	}

	if len(e.CommandsAllowed) > 0 {
		var disallowed []string
		for _, c := range r.Commands {
			if !matchesAnyCmd(e.CommandsAllowed, c) {
				disallowed = append(disallowed, c)
			}
		}
		out = append(out, assert("commands_allowed", len(disallowed) == 0,
			fmt.Sprintf("not allowed: %v", disallowed)))
	}

	if len(e.CommandsForbidden) > 0 {
		var hit []string
		for _, c := range r.Commands {
			if matchesAnyCmd(e.CommandsForbidden, c) {
				hit = append(hit, c)
			}
		}
		out = append(out, assert("commands_forbidden", len(hit) == 0,
			fmt.Sprintf("forbidden ran: %v", hit)))
	}

	for _, pat := range e.OutputMatches {
		re, err := regexp.Compile(pat)
		if err != nil {
			out = append(out, assert("output_matches "+pat, false, "bad regex: "+err.Error()))
			continue
		}
		out = append(out, assert("output_matches "+pat, re.MatchString(r.Output), "no match"))
	}

	if e.MaxTokens > 0 {
		out = append(out, assert("max_tokens", r.TokensUsed <= e.MaxTokens,
			fmt.Sprintf("used %d > %d", r.TokensUsed, e.MaxTokens)))
	}

	return out
}

// Passed reports whether all assertions passed.
func Passed(as []Assertion) bool {
	for _, a := range as {
		if !a.Pass {
			return false
		}
	}
	return true
}

func assert(name string, pass bool, failDetail string) Assertion {
	if pass {
		return Assertion{Name: name, Pass: true}
	}
	return Assertion{Name: name, Pass: false, Detail: failDetail}
}

func matchesAnyPath(patterns []string, s string) bool {
	for _, p := range patterns {
		if pathMatch(p, s) {
			return true
		}
	}
	return false
}

func matchesAnyCmd(patterns []string, s string) bool {
	for _, p := range patterns {
		if cmdMatch(p, s) {
			return true
		}
	}
	return false
}

// pathMatch matches a file path against a glob where `*` matches within one
// segment, `**` (and `**/`) crosses segments (matching zero or more), and `?`
// matches one non-slash char. e.g. "src/**/*.go" matches "src/main.go" and
// "src/a/b/main.go" but "src/*.go" does not match "src/a/main.go".
func pathMatch(pattern, s string) bool { return pathRegexp(pattern).MatchString(s) }

// cmdMatch matches a command string against a glob where `*` matches anything
// (including spaces and slashes), so "go test *" matches "go test ./...".
func cmdMatch(pattern, s string) bool { return cmdRegexp(pattern).MatchString(s) }

var reCache = map[string]*regexp.Regexp{}

func cached(key, expr string) *regexp.Regexp {
	if re, ok := reCache[key]; ok {
		return re
	}
	re := regexp.MustCompile(expr)
	reCache[key] = re
	return re
}

func pathRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++ // consume second '*'
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++ // consume trailing '/'; zero-or-more segments
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return cached("p:"+pattern, b.String())
}

func cmdRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")
	return cached("c:"+pattern, b.String())
}
