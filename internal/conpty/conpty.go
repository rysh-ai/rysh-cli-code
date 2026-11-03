// SPDX-License-Identifier: Apache-2.0

// Package conpty is the Windows pseudo-console (ConPTY) seam for rysh panes.
//
// On Unix every pane is a creack/pty shell; creack/pty's Windows build is a
// compile-only stub. This package is the replacement for that one call on
// Windows: Start creates a pseudo-console, runs the shell attached to it, and
// hands back a Console that reads, writes, resizes and closes. The pane actor
// uses it through its paneTerm seam (internal/actors/pane_term_windows.go).
//
// The Windows-only half lives in conpty_windows.go and has not been run on a
// real Windows host (design 085). Everything here is plain logic — size
// limits, shell choice and prompt hooks, the environment block — so it is
// unit-tested on any OS. Off Windows, Start returns ErrUnsupported.
package conpty

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
)

// ErrUnsupported is returned when the host has no pseudo-console: any OS other
// than Windows, and Windows before 10 1809, where ConPTY first shipped.
var ErrUnsupported = fmt.Errorf("conpty: no pseudo-console on this host (needs Windows 10 1809 or later): %w",
	errors.ErrUnsupported)

// Size is a pseudo-console size in character cells.
type Size struct {
	Rows uint16
	Cols uint16
}

// maxDim is the largest dimension ConPTY accepts: sizes travel as a COORD,
// whose fields are int16.
const maxDim = 32767

// NewSize validates a size for ConPTY. Zero is refused as well as overflow:
// CreatePseudoConsole and ResizePseudoConsole both reject an empty console.
func NewSize(rows, cols int) (Size, error) {
	if rows < 1 || cols < 1 || rows > maxDim || cols > maxDim {
		return Size{}, fmt.Errorf("conpty: size %dx%d out of range (1..%d)", rows, cols, maxDim)
	}
	return Size{Rows: uint16(rows), Cols: uint16(cols)}, nil
}

// Kind says how a shell must be started.
type Kind int

const (
	// KindOther is any shell rysh has no special handling for.
	KindOther Kind = iota
	// KindPowerShell is pwsh.exe (PowerShell 7) or powershell.exe (5.1).
	KindPowerShell
	// KindCmd is cmd.exe.
	KindCmd
	// KindPOSIX is a POSIX shell built for Windows (Git Bash, MSYS2).
	KindPOSIX
)

// Shell is the program a pane runs and the arguments it needs.
type Shell struct {
	Path string
	Args []string
	Kind Kind
}

// ResolveShell picks the pane shell on Windows.
//
// A configured shell wins, unless it is a POSIX absolute path: config's
// shellDefault falls back to /bin/bash when $SHELL is unset, which is the
// normal case on Windows and names nothing there. Otherwise the order is
// PowerShell 7, Windows PowerShell, %ComSpec%, cmd.exe.
//
// interactive is the pane's InteractiveShell setting. Only a POSIX shell gets
// -i for it: PowerShell is interactive by default and cmd has no such flag,
// and the actor's unconditional `-i` (pane_shell.go) is wrong for both.
func ResolveShell(configured string, interactive bool, getenv func(string) string,
	lookPath func(string) (string, error)) (Shell, error) {
	if c := strings.TrimSpace(configured); c != "" && !strings.HasPrefix(c, "/") {
		path := c
		if !strings.ContainsAny(c, `\/:`) {
			name := strings.ToLower(c)
			if !strings.Contains(name, ".") {
				name += ".exe"
			}
			p, err := lookPath(name)
			if err != nil {
				return Shell{}, fmt.Errorf("conpty: configured shell %q not found: %w", c, err)
			}
			path = p
		}
		return shellFor(path, interactive), nil
	}
	for _, name := range []string{"pwsh.exe", "powershell.exe"} {
		if p, err := lookPath(name); err == nil {
			return shellFor(p, interactive), nil
		}
	}
	if p := strings.TrimSpace(getenv("ComSpec")); p != "" {
		return shellFor(p, interactive), nil
	}
	if p, err := lookPath("cmd.exe"); err == nil {
		return shellFor(p, interactive), nil
	}
	return Shell{}, errors.New("conpty: no shell found (tried pwsh.exe, powershell.exe, %ComSpec%, cmd.exe)")
}

func shellFor(path string, interactive bool) Shell {
	base := strings.ToLower(path[strings.LastIndexAny(path, `\/`)+1:])
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "pwsh", "powershell":
		return Shell{Path: path, Args: []string{"-NoLogo"}, Kind: KindPowerShell}
	case "cmd":
		return Shell{Path: path, Kind: KindCmd}
	case "bash", "sh", "zsh", "dash", "fish":
		s := Shell{Path: path, Kind: KindPOSIX}
		if interactive {
			s.Args = []string{"-i"}
		}
		return s
	}
	return Shell{Path: path, Kind: KindOther}
}

// psPromptHook wraps whatever prompt the user's profile defined so that it
// first reports the working directory as OSC 7, which the pane parses for its
// live cwd (internal/actors/osc7.go). [char]27 rather than `e because Windows
// PowerShell 5.1 has no `e escape. The path is sent with forward slashes after
// the host — file://localhost/C:/Users/me — and the parser turns it back into
// C:\Users\me. Non-filesystem locations (HKLM:, Env:) report nothing.
const psPromptHook = `$global:__ryshPrompt = $function:prompt
function global:prompt {
  $l = $executionContext.SessionState.Path.CurrentLocation
  $osc = ''
  if ($l.Provider.Name -eq 'FileSystem') {
    $osc = "$([char]27)]7;file://localhost/$($l.ProviderPath -replace '\\','/')$([char]27)\"
  }
  $osc + (& $global:__ryshPrompt)
}`

// psLegacyPrompt is legacy (non-interactive) mode's minimal prompt, which rysh
// strips from the output exactly as it does bash's PS1="$ ".
const psLegacyPrompt = "function global:prompt { '$ ' }"

// cmdPromptHook is cmd.exe's PROMPT: $E is ESC, $P the drive and path, $G '>'.
// It reports the cwd as OSC 7 and then draws cmd's default "C:\dir>" prompt.
const cmdPromptHook = `$E]7;file://localhost/$P$E\$P$G`

// ShellHook adds what makes a pane shell report its working directory (OSC 7)
// and, in legacy mode, draw the minimal "$ " prompt rysh strips. It returns
// the shell with any extra arguments and the environment entries to add.
//
// PowerShell takes a script after its profile has loaded (-NoExit keeps it
// interactive), encoded so no quoting survives to CreateProcess. cmd.exe reads
// PROMPT from its environment. A POSIX shell gets the same PROMPT_COMMAND and
// PS1 rysh sets for bash on Unix. Any other shell is left alone.
func ShellHook(s Shell, interactive bool) (Shell, []string) {
	out := s
	out.Args = append([]string(nil), s.Args...)
	switch s.Kind {
	case KindPowerShell:
		script := psLegacyPrompt
		if interactive {
			script = psPromptHook
		}
		out.Args = append(out.Args, "-NoExit", "-EncodedCommand", encodePowerShell(script))
		return out, nil
	case KindCmd:
		if interactive {
			return out, []string{"PROMPT=" + cmdPromptHook}
		}
		return out, []string{"PROMPT=$$$S"}
	case KindPOSIX:
		if interactive {
			return out, []string{`PROMPT_COMMAND=printf '\033]7;file://%s%s\033\\' "$HOSTNAME" "$PWD"`}
		}
		return out, []string{"PS1=$ "}
	}
	return out, nil
}

// encodePowerShell encodes a script for -EncodedCommand: base64 of UTF-16LE.
func encodePowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// envKey returns the variable name of a NAME=value entry, or "" if it has
// none. Windows keeps per-drive working directories as "=C:=C:\dir", so a
// leading '=' belongs to the name.
func envKey(kv string) string {
	start := 0
	if strings.HasPrefix(kv, "=") {
		start = 1
	}
	if i := strings.IndexByte(kv[start:], '='); i >= 0 {
		return kv[:start+i]
	}
	return ""
}

// MergeEnv builds a child environment from base on Windows rules: names are
// case-insensitive, so "Path" and "PATH" are one variable.
//
// Entries whose names are in drop are removed. Each override replaces the
// variable of the same name in place (taking the override's spelling) or is
// appended. Entries with no name are dropped. os/exec does this de-duplication
// itself on Windows, but a ConPTY child is started with CreateProcess
// directly, so rysh has to.
func MergeEnv(base []string, drop []string, overrides ...string) []string {
	dropped := make(map[string]bool, len(drop))
	for _, d := range drop {
		dropped[strings.ToUpper(d)] = true
	}
	var out []string
	at := map[string]int{}
	set := func(kv string) {
		k := envKey(kv)
		if k == "" {
			return
		}
		uk := strings.ToUpper(k)
		if dropped[uk] {
			return
		}
		if i, ok := at[uk]; ok {
			out[i] = kv
			return
		}
		at[uk] = len(out)
		out = append(out, kv)
	}
	for _, kv := range base {
		set(kv)
	}
	for _, kv := range overrides {
		set(kv)
	}
	return out
}

// EnvBlock encodes env as the block CreateProcess takes with
// CREATE_UNICODE_ENVIRONMENT: UTF-16 NAME=value strings, each NUL-terminated,
// sorted by name case-insensitively as Windows documents, ending in an extra
// NUL. An empty environment is two NULs.
func EnvBlock(env []string) ([]uint16, error) {
	sorted := append([]string(nil), env...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return strings.ToUpper(envKey(sorted[i])) < strings.ToUpper(envKey(sorted[j]))
	})
	var block []uint16
	for _, kv := range sorted {
		if strings.IndexByte(kv, 0) >= 0 {
			return nil, fmt.Errorf("conpty: environment entry contains NUL")
		}
		block = append(block, utf16.Encode([]rune(kv))...)
		block = append(block, 0)
	}
	if len(block) == 0 {
		block = append(block, 0)
	}
	return append(block, 0), nil
}
