package agentic

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// buildEnvBlock fills the env-block template with concrete values for the
// current session. Returns "" if the template is empty (template gates the
// feature; an empty file effectively disables env-block injection).
//
// The function intentionally fans out a handful of lightweight, time-bounded
// shell calls (git status / branch) instead of importing a git library —
// rysh-cli already shells out for tools and the work happens once per Setup.
//
// Phase 2 item M.
func buildEnvBlock(template, workDir string) string {
	if strings.TrimSpace(template) == "" {
		return ""
	}
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		} else {
			workDir = "."
		}
	}

	branch, dirty := gitBranchAndDirty(workDir)
	dirtyMarker := ""
	if branch != "" && dirty {
		dirtyMarker = " (dirty)"
	}
	if branch == "" {
		branch = "<no git>"
	}

	substitutions := map[string]string{
		"{{cwd}}":          workDir,
		"{{os}}":           runtime.GOOS,
		"{{arch}}":         runtime.GOARCH,
		"{{date}}":         time.Now().Format("2006-01-02"),
		"{{git_branch}}":   branch,
		"{{git_dirty}}":    dirtyMarker,
		"{{project_type}}": detectProjectType(workDir),
		"{{tree}}":         shallowTree(workDir, 2, 60),
	}

	out := template
	for k, v := range substitutions {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}

// gitBranchAndDirty returns the current branch name and a dirty flag. Empty
// branch + false means "not a git repo or git not on PATH".
func gitBranchAndDirty(workDir string) (string, bool) {
	branch := runGit(workDir, 800*time.Millisecond, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		return "", false
	}
	// porcelain returns non-empty when working tree has uncommitted changes;
	// the simplest dirty check.
	status := runGit(workDir, 1500*time.Millisecond, "status", "--porcelain")
	return strings.TrimSpace(branch), strings.TrimSpace(status) != ""
}

func runGit(workDir string, timeout time.Duration, args ...string) string {
	if _, err := exec.LookPath("git"); err != nil {
		return ""
	}
	ctx, cancel := newContext(timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil // discard
	if err := cmd.Run(); err != nil {
		return ""
	}
	return out.String()
}

// detectProjectType inspects a handful of canonical marker files at workDir
// and returns a short human-readable label. Order matters: the first match
// wins, so more specific markers come first.
func detectProjectType(workDir string) string {
	checks := []struct {
		file  string
		label string
	}{
		{"go.work", "Go workspace"},
		{"go.mod", "Go module"},
		{"package.json", "Node.js / npm"},
		{"pyproject.toml", "Python (pyproject)"},
		{"requirements.txt", "Python (requirements.txt)"},
		{"Cargo.toml", "Rust (cargo)"},
		{"pom.xml", "Java (Maven)"},
		{"build.gradle", "Java (Gradle)"},
		{"build.gradle.kts", "Kotlin (Gradle)"},
		{"composer.json", "PHP (composer)"},
		{"Gemfile", "Ruby (bundler)"},
		{"mix.exs", "Elixir (mix)"},
		{"dune-project", "OCaml (dune)"},
		{"flake.nix", "Nix flake"},
	}
	for _, c := range checks {
		if _, err := os.Stat(filepath.Join(workDir, c.file)); err == nil {
			return c.label
		}
	}
	return "unrecognised"
}

// shallowTree returns a compact text representation of the workDir layout
// limited to maxDepth and at most maxLines entries. Hidden files / typical
// vendor directories are skipped so the env block stays readable.
//
// The format is intentionally simple ("dir/" suffix, no fancy connectors) so
// the model parses it reliably without learning a custom tree-art convention.
func shallowTree(workDir string, maxDepth, maxLines int) string {
	skip := map[string]bool{
		".git":         true,
		"node_modules": true,
		"vendor":       true,
		"dist":         true,
		"build":        true,
		"target":       true,
		".idea":        true,
		".vscode":      true,
		"__pycache__":  true,
	}
	var lines []string
	var walk func(path, prefix string, depth int) bool
	walk = func(path, prefix string, depth int) bool {
		if depth > maxDepth {
			return true
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return true
		}
		// Stable sort: directories first, then files, both alphabetical.
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
		for _, e := range entries {
			if len(lines) >= maxLines {
				lines = append(lines, prefix+"... (truncated)")
				return false
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") && name != ".github" {
				continue
			}
			if skip[name] {
				continue
			}
			label := name
			if e.IsDir() {
				label += "/"
			}
			lines = append(lines, prefix+label)
			if e.IsDir() {
				if !walk(filepath.Join(path, name), prefix+"  ", depth+1) {
					return false
				}
			}
		}
		return true
	}
	walk(workDir, "", 1)
	if len(lines) == 0 {
		return "(empty)"
	}
	return strings.Join(lines, "\n")
}
