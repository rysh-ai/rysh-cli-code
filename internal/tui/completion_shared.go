package tui

// completion_shared.go — an exported facade over the TUI's shell tab-completion
// engine (completion.go + completion_bash.go) so other in-process frontends can
// reuse it. The web server (internal/web, roadmap task W7) serves browser-mode
// tab-completion through this: the daemon owns the pane's shell, so the exact
// completion logic the TUI runs locally answers the same request server-side.
//
// Deliberately a thin wrapper: no logic is duplicated here, only argument
// resolution (cwd from the reported OSC 7 value or the shell pid) and the
// command/bash/path dispatch that Model.completeShellInput performs inline.

import (
	"os"
	"strings"
)

// ShellCompletionCandidate is one completion candidate: the full replacement
// token (e.g. "src/model.go" — directory part preserved, no trailing slash)
// plus whether it names a directory (the caller appends "/" instead of a space
// when inserting it).
type ShellCompletionCandidate struct {
	Value string `json:"value"`
	IsDir bool   `json:"is_dir"`
}

// ShellCompletionRequest carries one completion request for the token under
// the cursor of a shell input line.
type ShellCompletionRequest struct {
	Token        string // the (possibly empty) token being completed
	Line         string // full input line up to the cursor (bash programmable completion)
	Cwd          string // OSC 7-reported live cwd ("" = resolve from ShellPID)
	ShellPID     int    // pane shell pid, used when Cwd is empty
	IsFirstToken bool   // token is in command position
	// UseBashCompletion enables delegation to bash's programmable completion
	// specs in argument position (config [ui] shell_bash_completion).
	UseBashCompletion bool
}

// ShellCompletions computes candidates for req, mirroring the TUI's Tab
// behavior: command-name completion ($PATH + builtins) in command position,
// bash programmable completion (when enabled) then file/path completion in
// argument position. Best-effort and read-only; returns nil when nothing
// matches.
func ShellCompletions(req ShellCompletionRequest) []ShellCompletionCandidate {
	token := req.Token
	// A path-ish token ("./x", "~/x", "a/b") never completes as a command name,
	// even in the first position (mirrors completeShellInput's isCommand check
	// and the desktop app's isPath check).
	isPath := strings.Contains(token, "/") || strings.HasPrefix(token, "~") || strings.HasPrefix(token, ".")
	if req.IsFirstToken && !isPath {
		return toCandidates(commandCandidates(token))
	}

	cwd := resolveCompletionCwd(req.Cwd, req.ShellPID)

	// Argument position: bash's programmable completion first (git branches,
	// ssh hosts, docker verbs, flags); built-in path completion as fallback.
	if req.UseBashCompletion && strings.TrimSpace(req.Line) != "" {
		if prog := bashCompletionCandidates(cwd, req.Line); len(prog) > 0 {
			return toCandidates(markDirCandidates(prog, cwd))
		}
	}
	return toCandidates(pathCandidates(token, cwd))
}

// resolveCompletionCwd picks the directory path completion resolves against:
// the reported cwd when present (OSC 7, push-based and exact), else the live
// cwd of the shell process, else this process's cwd.
func resolveCompletionCwd(reported string, shellPID int) string {
	if strings.TrimSpace(reported) != "" {
		return reported
	}
	if shellPID > 0 {
		if dir, ok := processCwd(shellPID); ok {
			return dir
		}
	}
	if dir, err := os.Getwd(); err == nil {
		return dir
	}
	return "."
}

// toCandidates converts the engine's string candidates (trailing "/" marks a
// directory) into the exported struct form (no trailing slash + IsDir flag),
// matching the desktop app's completion.get reply shape.
func toCandidates(values []string) []ShellCompletionCandidate {
	if len(values) == 0 {
		return nil
	}
	out := make([]ShellCompletionCandidate, 0, len(values))
	for _, v := range values {
		if isDir := strings.HasSuffix(v, "/"); isDir && v != "/" {
			out = append(out, ShellCompletionCandidate{Value: strings.TrimSuffix(v, "/"), IsDir: true})
		} else {
			out = append(out, ShellCompletionCandidate{Value: v, IsDir: isDir})
		}
	}
	return out
}
