// SPDX-License-Identifier: Apache-2.0

package tui

// model_shellprompt.go — context-aware shell prompt and PS2 continuation
// (Phase 5 of bash-shell-mode).
//
// The shell-mode input line shows where you are (like a real PS1) instead of
// a static "> ": the prompt template ([ui] shell_prompt, default "{dir} > ")
// is rendered from the pane's live OSC 7-reported cwd. When a submitted line
// is syntactically incomplete (trailing backslash, unbalanced quotes), the
// input switches to a PS2-style "> " continuation prompt and accumulates
// lines until the logical command is complete — which is then executed AND
// recorded in history as a single entry, like bash's cmdhist.

import (
	"os"
	"path/filepath"
	"strings"
)

// shellPromptFor renders the shell-mode prompt for a pane from the config
// template and the pane's live cwd. Falls back to a plain "> " when the cwd
// is unknown (no OSC 7 report yet) or the rendered prompt would crowd the
// input line.
func (m Model) shellPromptFor(paneID string, width int) string {
	const plain = "> "
	pane := m.findPaneInSnapshot(paneID)
	if pane == nil || pane.ShellCwd == "" {
		return plain
	}
	tmpl := m.cfg.ShellPrompt
	if tmpl == "" {
		tmpl = "{dir} > "
	}

	cwd := pane.ShellCwd
	home, _ := os.UserHomeDir()
	abbrev := cwd
	dir := filepath.Base(cwd)
	if home != "" {
		if cwd == home {
			abbrev, dir = "~", "~"
		} else if rel, found := strings.CutPrefix(cwd, home+"/"); found {
			abbrev = "~/" + rel
		}
	}

	out := strings.ReplaceAll(tmpl, "{cwd}", abbrev)
	out = strings.ReplaceAll(out, "{dir}", dir)
	out = strings.ReplaceAll(out, "{session}", m.cfg.SessionName)

	// Never let the prompt eat the input line: cap it at a third of the
	// pane width (min 8 columns), degrading to the plain prompt.
	maxLen := width / 3
	if maxLen < 8 {
		maxLen = 8
	}
	if len([]rune(out)) > maxLen {
		return plain
	}
	return out
}

// shellCommandIncomplete reports whether a shell command line is
// syntactically unfinished — an unclosed single/double quote or a trailing
// line-continuation backslash — so Enter should open a PS2 continuation
// instead of executing. Comments are skipped (quotes inside them don't
// count), and backslashes inside single quotes are literal, matching bash.
// Keyword-level continuation (for/do without done, unclosed heredocs) is
// intentionally left to the shell's own PS2 through the PTY.
func shellCommandIncomplete(s string) bool {
	inSingle, inDouble, escaped, inComment := false, false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inComment {
			if c == '\n' {
				inComment = false
			}
			continue
		}
		switch {
		case c == '\\' && !inSingle:
			escaped = true
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble &&
			(i == 0 || s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '\n' || s[i-1] == ';'):
			inComment = true
		}
	}
	// A trailing backslash escapes the (implicit) newline → continuation.
	return escaped || inSingle || inDouble
}
