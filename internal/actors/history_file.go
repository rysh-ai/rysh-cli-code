package actors

// history_file.go — persistent shell history (Phase 3 of bash-shell-mode).
//
// Shell commands are appended to a bash_history-style file at
// <RyshDir>/history/<session>.history. All panes of a session share the file
// (like bash terminals sharing HISTFILE); a fresh pane (no KV-restored
// history) seeds its in-memory history from the file, so up-arrow recall and
// Ctrl+R search survive session restarts.
//
// Entries are one per line. Multi-line commands stay single entries via
// backslash escaping (\n and \\), which is the identity for typical commands
// so the file remains greppable/bash-readable.
//
// Multiple PaneActors (goroutines in one process) append concurrently — a
// package-level mutex serializes file access. This is the one sanctioned
// exception pattern to the no-mutexes-in-actors rule: the lock guards an OS
// resource shared across actors, never actor state.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

var historyFileMu sync.Mutex

// defaultShellHistorySize is the fallback when config carries no positive
// shell_history_size (e.g. zero-value configs in tests).
const defaultShellHistorySize = 1000

func shellHistorySize(cfg config.Config) int {
	if cfg.ShellHistorySize > 0 {
		return cfg.ShellHistorySize
	}
	return defaultShellHistorySize
}

// historyFilePath returns the session's shell history file, or "" when no
// rysh state directory is configured (nothing to persist to).
func historyFilePath(cfg config.Config) string {
	dir := strings.TrimSpace(cfg.RyshDir)
	if dir == "" {
		return ""
	}
	session := strings.TrimSpace(cfg.SessionName)
	if session == "" {
		session = "default"
	}
	return filepath.Join(dir, "history", session+".history")
}

// escapeHistoryEntry folds a (possibly multi-line) command onto one line.
func escapeHistoryEntry(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// unescapeHistoryEntry reverses escapeHistoryEntry.
func unescapeHistoryEntry(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// appendHistoryFile records one executed shell command. Failures are silent —
// history persistence must never break command execution.
func appendHistoryFile(cfg config.Config, cmd string) {
	path := historyFilePath(cfg)
	if path == "" || cmd == "" {
		return
	}
	historyFileMu.Lock()
	defer historyFileMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(escapeHistoryEntry(cmd) + "\n")
}

// loadHistoryFile returns the newest `limit` entries (oldest first, matching
// the in-memory history order). When the file has grown past the limit it is
// opportunistically rewritten trimmed — the bounded-HISTFILESIZE behavior —
// so the file cannot grow without bound across sessions.
func loadHistoryFile(cfg config.Config, limit int) []string {
	path := historyFilePath(cfg)
	if path == "" {
		return nil
	}
	historyFileMu.Lock()
	defer historyFileMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	entries := make([]string, 0, len(lines))
	for _, ln := range lines {
		if ln != "" {
			entries = append(entries, unescapeHistoryEntry(ln))
		}
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
		var b strings.Builder
		for _, e := range entries {
			b.WriteString(escapeHistoryEntry(e))
			b.WriteByte('\n')
		}
		_ = os.WriteFile(path, []byte(b.String()), 0o600)
	}
	return entries
}
