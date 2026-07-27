package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// completionSession is the state of an in-progress shell tab-completion menu.
// The candidate list is shown above the pane's prompt; pressing Tab again
// cycles index through candidates, inserting each into the input line (menu
// completion). index == -1 means only the common prefix has been filled in and
// nothing is selected yet.
type completionSession struct {
	active     bool     // a menu is currently shown for paneID
	paneID     string   // pane the menu belongs to
	prefixPart string   // input text before the token being completed
	suffix     string   // input text after the cursor
	candidates []string // full replacement tokens (e.g. "src/model.go")
	names      []string // display basenames (e.g. "model.go")
	index      int      // selected candidate, or -1 when only the prefix is shown
}

// completeShellInput performs readline-style tab-completion on the active pane's
// shell-mode input line, operating on the token under the cursor:
//
//   - First token (and no slash) → command-name completion from $PATH + builtins.
//   - Otherwise → file/directory path completion, resolved against the pane's
//     live working directory.
//
// A single match is inserted in full (trailing "/" for directories, space
// otherwise). Multiple matches fill in the longest common prefix and open an
// in-pane menu; subsequent calls cycle the selection through the candidates
// (reverse steps backward) without re-reading the filesystem.
func (m Model) completeShellInput(reverse bool) (tea.Model, tea.Cmd) {
	paneID := m.snapshot.ActivePaneID
	input, ok := m.inputs[paneID]
	if !ok {
		return m, nil
	}

	// Continuation: a menu is already open for this pane → advance the
	// selection and insert the highlighted candidate.
	if m.completion.active && m.completion.paneID == paneID && len(m.completion.candidates) > 0 {
		s := &m.completion
		n := len(s.candidates)
		switch {
		case reverse && s.index < 0:
			s.index = n - 1
		case reverse:
			s.index = (s.index - 1 + n) % n
		default:
			s.index = (s.index + 1) % n
		}
		newBefore := s.prefixPart + s.candidates[s.index]
		input.SetValue(newBefore + s.suffix)
		input.SetCursor(len([]rune(newBefore)))
		m.inputs[paneID] = input
		return m, nil
	}

	// Shift+Tab with no open menu has nothing to cycle backward through.
	if reverse {
		return m, nil
	}

	runes := []rune(input.Value())
	pos := input.Position()
	if pos < 0 || pos > len(runes) {
		pos = len(runes)
	}
	before := string(runes[:pos])
	after := string(runes[pos:])

	// The token under the cursor starts after the last space in `before`.
	tokStart := strings.LastIndex(before, " ") + 1
	token := before[tokStart:]
	prefixPart := before[:tokStart] // command + args typed so far (with trailing space)

	// A leading token with no path separator is a command name.
	isCommand := strings.TrimSpace(prefixPart) == "" && !strings.Contains(token, "/")

	var candidates []string
	if isCommand {
		candidates = commandCandidates(token)
	} else {
		cwd := m.activePaneCwd()
		// Argument position: ask bash's programmable completion first —
		// subcommands, branches, hosts, flags (git/ssh/docker/...). Falls
		// back to built-in file/directory completion when the command has
		// no spec, the sidecar fails, or the feature is disabled.
		if m.cfg.ShellBashCompletion {
			if prog := bashCompletionCandidates(cwd, before); len(prog) > 0 {
				candidates = markDirCandidates(prog, cwd)
			}
		}
		if len(candidates) == 0 {
			candidates = pathCandidates(token, cwd)
		}
	}
	if len(candidates) == 0 {
		return m, nil
	}

	if len(candidates) == 1 {
		newToken := candidates[0]
		if !strings.HasSuffix(newToken, "/") {
			newToken += " "
		}
		newBefore := prefixPart + newToken
		input.SetValue(newBefore + after)
		input.SetCursor(len([]rune(newBefore)))
		m.inputs[paneID] = input
		return m, nil
	}

	// Multiple matches: fill in the common prefix and open the menu (nothing
	// selected yet). The candidate list renders above the prompt; the next Tab
	// starts cycling from the first candidate.
	newBefore := prefixPart + longestCommonPrefix(candidates)
	input.SetValue(newBefore + after)
	input.SetCursor(len([]rune(newBefore)))
	m.inputs[paneID] = input

	m.completion = completionSession{
		active:     true,
		paneID:     paneID,
		prefixPart: prefixPart,
		suffix:     after,
		candidates: candidates,
		names:      candidateNames(candidates),
		index:      -1,
	}
	return m, nil
}

// cwdCacheTTL bounds how long a resolved shell CWD is reused before the TUI
// re-resolves it. Kept short so background directory changes are picked up
// quickly; explicit cd via the prompt invalidates the entry immediately.
const cwdCacheTTL = 3 * time.Second

// cwdCacheEntry is a per-pane cached shell working directory. pid is recorded so
// a restarted shell (new pid) invalidates the entry automatically.
type cwdCacheEntry struct {
	pid       int
	dir       string
	fetchedAt time.Time
}

// invalidatePaneCwd drops the cached CWD for a pane, forcing the next
// completion to re-resolve it. Called when a shell command is submitted.
func (m *Model) invalidatePaneCwd(paneID string) {
	if m.paneCwdCache != nil {
		delete(m.paneCwdCache, paneID)
	}
}

// activePaneCwd resolves the working directory used for path completion: the
// live CWD of the active pane's shell when resolvable, else this process's CWD.
// Results are cached per pane (keyed by pane ID, validated against the shell
// pid) for cwdCacheTTL to avoid an lsof/proc lookup on every Tab press.
func (m Model) activePaneCwd() string {
	paneID := m.snapshot.ActivePaneID
	pane := m.findPaneInSnapshot(paneID)
	// OSC 7-reported cwd is push-based and exact — no cache or lsof needed.
	if pane != nil && pane.ShellCwd != "" {
		return pane.ShellCwd
	}
	if pane != nil && pane.ShellPID > 0 {
		if m.paneCwdCache != nil {
			if e, ok := m.paneCwdCache[paneID]; ok &&
				e.pid == pane.ShellPID && time.Since(e.fetchedAt) < cwdCacheTTL {
				return e.dir
			}
		}
		if dir, ok := processCwd(pane.ShellPID); ok {
			if m.paneCwdCache != nil {
				m.paneCwdCache[paneID] = cwdCacheEntry{pid: pane.ShellPID, dir: dir, fetchedAt: time.Now()}
			}
			return dir
		}
	}
	if dir, err := os.Getwd(); err == nil {
		return dir
	}
	return "."
}

// shellBuiltins is a common subset of POSIX/bash builtins offered for command
// completion (these never appear as files in $PATH).
var shellBuiltins = []string{
	"alias", "bg", "cd", "command", "declare", "echo", "eval", "exec", "exit",
	"export", "fg", "history", "jobs", "kill", "let", "local", "logout", "popd",
	"pushd", "pwd", "read", "readonly", "return", "set", "shift", "source",
	"test", "times", "trap", "type", "ulimit", "umask", "unalias", "unset", "wait",
}

// commandCandidates returns executable names in $PATH (plus shell builtins) that
// start with prefix.
func commandCandidates(prefix string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(name string) {
		if name != "" && strings.HasPrefix(name, prefix) && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}

	for _, b := range shellBuiltins {
		add(b)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			info, err := e.Info()
			if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
				continue
			}
			add(name)
		}
	}
	sort.Strings(out)
	return out
}

// pathCandidates returns filesystem entries matching the (possibly partial)
// path token, resolved relative to cwd. Each candidate preserves the directory
// portion the user typed and carries a trailing "/" for directories.
func pathCandidates(token, cwd string) []string {
	dirPart := ""
	base := token
	if i := strings.LastIndex(token, "/"); i >= 0 {
		dirPart = token[:i+1] // keep the trailing slash
		base = token[i+1:]
	}

	entries, err := os.ReadDir(expandPath(dirPart, cwd))
	if err != nil {
		return nil
	}

	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		// Only surface dotfiles when the user explicitly typed a leading dot.
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		cand := dirPart + name
		if isDirEntry(e, expandPath(dirPart, cwd)) {
			cand += "/"
		}
		out = append(out, cand)
	}
	sort.Strings(out)
	return out
}

// isDirEntry reports whether the entry is a directory, following symlinks so a
// symlinked directory still completes with a trailing slash.
func isDirEntry(e os.DirEntry, dir string) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink != 0 {
		if info, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
			return info.IsDir()
		}
	}
	return false
}

// expandPath turns the directory portion of a completion token into an absolute
// filesystem path: it expands a leading ~, then resolves relative paths against
// cwd. An empty dirPart means "the current directory".
func expandPath(dirPart, cwd string) string {
	p := dirPart
	if p == "" {
		return cwd
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return p
}

// longestCommonPrefix returns the longest string that is a prefix of every
// element of ss, compared rune by rune.
func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	prefix := []rune(ss[0])
	for _, s := range ss[1:] {
		r := []rune(s)
		n := len(prefix)
		if len(r) < n {
			n = len(r)
		}
		i := 0
		for i < n && prefix[i] == r[i] {
			i++
		}
		prefix = prefix[:i]
		if len(prefix) == 0 {
			break
		}
	}
	return string(prefix)
}

// candidateNames reduces full completion tokens to their display basenames,
// preserving the trailing "/" that marks directories.
func candidateNames(cands []string) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		name := c
		trimmed := strings.TrimSuffix(c, "/")
		if i := strings.LastIndex(trimmed, "/"); i >= 0 {
			name = c[i+1:]
		}
		out = append(out, name)
	}
	return out
}

// maxCompletionHintLines caps how many rows the in-pane candidate list may
// occupy so it never crowds out the pane's own output.
const maxCompletionHintLines = 6

// completionHintLines lays out the candidate names of the open completion menu
// into width-fit rows for rendering inside the active pane, just above its
// prompt. The selected candidate is highlighted, and the visible window of rows
// scrolls to keep the selection on screen when the list exceeds the row cap.
func (m Model) completionHintLines(width int) []string {
	names := m.completion.names
	if len(names) == 0 || width < 4 {
		return nil
	}
	sel := m.completion.index
	const gap = "  "

	// Greedily pack candidate indices into rows using plain (unstyled) widths.
	var rows [][]int
	var cur []int
	curLen := 0
	for i, name := range names {
		add := len(name)
		if len(cur) > 0 {
			add += len(gap)
		}
		if len(cur) > 0 && curLen+add > width {
			rows = append(rows, cur)
			cur, curLen = nil, 0
			add = len(name)
		}
		cur = append(cur, i)
		curLen += add
	}
	if len(cur) > 0 {
		rows = append(rows, cur)
	}

	// Window the rows so the selected candidate stays visible.
	start := 0
	if len(rows) > maxCompletionHintLines {
		selRow := 0
		for r, row := range rows {
			for _, idx := range row {
				if idx == sel {
					selRow = r
				}
			}
		}
		start = selRow - maxCompletionHintLines/2
		if start < 0 {
			start = 0
		}
		if start+maxCompletionHintLines > len(rows) {
			start = len(rows) - maxCompletionHintLines
		}
	}
	end := start + maxCompletionHintLines
	if end > len(rows) {
		end = len(rows)
	}

	out := make([]string, 0, end-start)
	for _, row := range rows[start:end] {
		parts := make([]string, len(row))
		for j, idx := range row {
			if idx == sel {
				parts[j] = completionSelStyle.Render(names[idx])
			} else {
				parts[j] = completionHintStyle.Render(names[idx])
			}
		}
		out = append(out, strings.Join(parts, gap))
	}
	return out
}
