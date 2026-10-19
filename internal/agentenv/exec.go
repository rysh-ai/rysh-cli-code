// SPDX-License-Identifier: Apache-2.0

package agentenv

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// ExecFunc replaces the current process. syscall.Exec in production; a test
// double captures the call instead.
type ExecFunc func(path string, argv []string, env []string) error

// Main implements `rysh exec-clean [--agent claude|codex] [--explain] -- cmd args…`
// and `rysh exec-clean --print-static`. It filters environ for the agent and
// replaces itself with cmd (the same pid: nothing is left running, and no
// NAME=VALUE ever appears in argv). It returns an exit code only when it does
// NOT exec (usage error, --print-static, a failed exec).
//
// --explain writes the dropped NAMES (never values), one per line, to stderr
// before the exec; it is for tests and for a person checking what was removed.
func Main(args, environ []string, stdout, stderr io.Writer, execFn ExecFunc) int {
	kind := Claude
	explain := false
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--":
			args = args[1:]
			goto run
		case a == "--print-static":
			fmt.Fprintln(stdout, StaticPrefix())
			return 0
		case a == "--explain":
			explain = true
			args = args[1:]
		case a == "--agent" && len(args) > 1:
			k, ok := ParseKind(args[1])
			if !ok {
				fmt.Fprintf(stderr, "exec-clean: unknown --agent %q (claude|codex)\n", args[1])
				return 2
			}
			kind = k
			args = args[2:]
		case strings.HasPrefix(a, "--agent="):
			k, ok := ParseKind(strings.TrimPrefix(a, "--agent="))
			if !ok {
				fmt.Fprintf(stderr, "exec-clean: unknown --agent %q (claude|codex)\n", strings.TrimPrefix(a, "--agent="))
				return 2
			}
			kind = k
			args = args[1:]
		default:
			goto run
		}
	}
run:
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: exec-clean [--agent claude|codex] [--explain] -- <command> [args…]")
		return 2
	}
	env := Filter(environ, kind)
	if explain {
		for _, n := range Dropped(environ, kind) {
			fmt.Fprintln(stderr, n)
		}
	}
	path, err := lookPath(args[0], env)
	if err != nil {
		fmt.Fprintf(stderr, "exec-clean: %v\n", err)
		return 127
	}
	if err := execFn(path, args, env); err != nil {
		fmt.Fprintf(stderr, "exec-clean: exec %s: %v\n", args[0], err)
		return 126
	}
	return 0 // only reached by a test double
}

// lookPath resolves name against the PATH of the FILTERED environment — the
// one the agent will run with (PATH is never filtered, so this is the caller's).
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		if _, err := os.Stat(name); err != nil {
			return "", err
		}
		return name, nil
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, dir := range filepath.SplitList(v) {
				if dir == "" {
					dir = "."
				}
				p := filepath.Join(dir, name)
				if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
					return p, nil
				}
			}
			return "", fmt.Errorf("%s: not found in PATH", name)
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return p, nil
}

// SysExec is the production ExecFunc.
func SysExec(path string, argv []string, env []string) error {
	return syscall.Exec(path, argv, env)
}

// --- typed lines ------------------------------------------------------------

var (
	binOnce sync.Once
	binPath string
)

// Binary is the absolute path of the running rysh executable — the binary that
// is typing the line, so it is guaranteed to have exec-clean. Overridable in
// tests via SetBinaryForTest.
func Binary() string {
	binOnce.Do(func() {
		if p, err := os.Executable(); err == nil {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				p = r
			}
			binPath = p
		}
	})
	return binPath
}

// SetBinaryForTest pins Binary (tests only).
func SetBinaryForTest(p string) { binOnce.Do(func() {}); binPath = p }

// TypedLine wraps cmd (a shell line starting with the agent binary) so the pane
// shell runs it through exec-clean, in ONE form for sh, bash, zsh and fish:
//
//	test -x BIN && BIN exec-clean --agent K -- CMD || test -x BIN || env -u … CMD
//
// When BIN exists the agent runs through exec-clean; if the agent itself exits
// non-zero, `test -x BIN` is true and nothing is re-run. Only a host WITHOUT the
// binary (a remote pane) takes the generated StaticPrefix fallback. No value can
// appear in the line: it carries names and the command only.
func TypedLine(kind Kind, cmd string) string {
	bin := Binary()
	if bin == "" {
		return StaticPrefix() + " " + cmd
	}
	q := quoteAnyShell(bin)
	return "test -x " + q + " && " + q + " exec-clean --agent " + string(kind) + " -- " + cmd +
		" || test -x " + q + " || " + StaticPrefix() + " " + cmd
}

// quoteAnyShell single-quotes s for sh, bash, zsh and fish alike (the same rule
// as actors.shellQuoteAnyShell: fish treats a backslash inside single quotes as
// an escape, so it is spelled '\\').
func quoteAnyShell(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			b.WriteString(`'\''`)
		case '\\':
			b.WriteString(`'\\'`)
		default:
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('\'')
	return b.String()
}
