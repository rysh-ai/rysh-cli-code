package tui

// completion_bash.go — bash programmable completion delegation (Phase 4 of
// bash-shell-mode).
//
// rysh's built-in completion covers command names ($PATH scan) and file
// paths. Everything a bash user gets beyond that — `git che<Tab>` →
// checkout/cherry-pick, ssh host completion, docker/kubectl subcommands —
// comes from bash-completion's programmable specs. Instead of reimplementing
// them, Tab in an argument position asks a real bash to run the command's
// completion function via a small driver script, in the pane's live cwd.
//
// Best-effort by design: a short timeout, silent failure, and the built-in
// path completion as fallback. Gated by `[ui] shell_bash_completion`.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// bashCompletionTimeout bounds how long a Tab press may wait on the bash
// sidecar. Completion functions are fast once bash-completion is loaded;
// sourcing it dominates (~100ms cold).
const bashCompletionTimeout = 400 * time.Millisecond

// maxBashCandidates caps the candidate list so a runaway completion function
// (e.g. every file in a huge repo) cannot flood the menu.
const maxBashCandidates = 200

// bashCompletionDriver runs a command's programmable completion spec the way
// bash itself would: reconstruct COMP_WORDS/COMP_CWORD from the line, load
// the spec (on demand via _completion_loader), call its -F function, and
// print COMPREPLY one entry per line. $1 is the input line up to the cursor.
const bashCompletionDriver = `
for f in /usr/share/bash-completion/bash_completion \
         /opt/homebrew/etc/profile.d/bash_completion.sh \
         /usr/local/etc/profile.d/bash_completion.sh \
         /etc/bash_completion; do
  if [ -r "$f" ]; then . "$f" 2>/dev/null && break; fi
done
COMP_LINE="$1"
COMP_POINT=${#COMP_LINE}
eval set -- "$COMP_LINE" 2>/dev/null || exit 0
COMP_WORDS=("$@")
COMP_CWORD=$((${#COMP_WORDS[@]}-1))
case "$COMP_LINE" in *' ')
  COMP_WORDS+=("")
  COMP_CWORD=$((COMP_CWORD+1))
;; esac
[ "$COMP_CWORD" -lt 1 ] && exit 0
cmd="${COMP_WORDS[0]}"
cur="${COMP_WORDS[COMP_CWORD]}"
prev="${COMP_WORDS[COMP_CWORD-1]}"
spec=$(complete -p "$cmd" 2>/dev/null)
if [ -z "$spec" ] && declare -F _completion_loader >/dev/null 2>&1; then
  _completion_loader "$cmd" 2>/dev/null
  spec=$(complete -p "$cmd" 2>/dev/null)
fi
[ -z "$spec" ] && exit 0
fn=$(printf '%s\n' "$spec" | sed -n 's/.*-F \([^ ]*\).*/\1/p')
[ -z "$fn" ] && exit 0
"$fn" "$cmd" "$cur" "$prev" 2>/dev/null
printf '%s\n' "${COMPREPLY[@]}"
`

// bashCompletionCandidates asks a bash sidecar for programmable completions
// of `line` (the input up to the cursor), in cwd. Returns nil on any failure
// or when the command has no completion spec — callers fall back to the
// built-in path completion.
func bashCompletionCandidates(cwd, line string) []string {
	line = strings.TrimLeft(line, " \t")
	if line == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), bashCompletionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", bashCompletionDriver, "rysh-complete", line)
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var cands []string
	for _, ln := range strings.Split(out.String(), "\n") {
		ln = strings.TrimRight(ln, " ")
		if ln == "" || seen[ln] {
			continue
		}
		seen[ln] = true
		cands = append(cands, ln)
		if len(cands) >= maxBashCandidates {
			break
		}
	}
	sort.Strings(cands)
	return cands
}

// markDirCandidates appends "/" to candidates that name directories (relative
// to cwd), matching the built-in path completion's convention so a completed
// directory keeps the cursor ready for the next path segment instead of
// inserting a space.
func markDirCandidates(cands []string, cwd string) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c
		if strings.HasSuffix(c, "/") || strings.ContainsAny(c, " \t") {
			continue
		}
		p := c
		if strings.HasPrefix(p, "~/") || p == "~" {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, strings.TrimPrefix(p, "~"))
			}
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			out[i] = c + "/"
		}
	}
	return out
}
