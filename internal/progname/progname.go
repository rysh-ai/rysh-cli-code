// SPDX-License-Identifier: Apache-2.0

// Package progname resolves the name this binary was invoked as, so that
// user-facing text names the command the user actually has.
//
// The same source ships under two names: `rysh` for the open-source build and
// `ry` for the closed one. Text is written with the literal "rysh"; Rewrite
// swaps in the real name at print time.
package progname

import (
	"os"
	"path/filepath"
	"strings"
)

// Name reports the name this binary was invoked as, falling back to "rysh" when
// argv[0] is unusable. A "*.test" name (the binary `go test` builds) also falls
// back, so output does not depend on the test harness.
func Name() string {
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, ".exe")
	if base == "" || base == "." || base == string(filepath.Separator) ||
		strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".test") {
		return "rysh"
	}
	return base
}

// Rewrite replaces the standalone word "rysh" with the invoked binary name.
//
// Only the bare command word is replaced. Anything touching '.', '/', '-' or
// '_' is a filename, path or identifier and keeps its real spelling —
// rysh.config.yaml, ./.rysh/, /tmp/rysh/logs/, rysh.lock, rysh-cli.
//
// Two further spellings are left alone because they are not the binary:
//
//	[rysh]    the prefix tagging output as coming from the multiplexer
//	##rysh    the in-session system command, named the same in both builds
//
// '[' and '#' are not name characters, so the boundary rules alone would have
// replaced these; taggedForm excludes them explicitly.
func Rewrite(s string) string {
	name := Name()
	if name == "rysh" {
		return s
	}

	const word = "rysh"
	var b strings.Builder
	for i := 0; ; {
		j := strings.Index(s[i:], word)
		if j < 0 {
			b.WriteString(s[i:])
			return b.String()
		}
		j += i
		b.WriteString(s[i:j])
		if standalone(s, j, len(word)) && !taggedForm(s, j) {
			b.WriteString(name)
		} else {
			b.WriteString(word)
		}
		i = j + len(word)
	}
}

// taggedForm reports whether the occurrence at start is "[rysh" or "##rysh" —
// the message prefix and the system command, neither of which is the binary.
func taggedForm(s string, start int) bool {
	if start > 0 && s[start-1] == '[' {
		return true
	}
	return start > 0 && s[start-1] == '#'
}

// standalone reports whether s[start:start+n] is delimited on both sides by
// something that cannot be part of a filename or identifier.
func standalone(s string, start, n int) bool {
	if start > 0 && !nameBoundary(s[start-1]) {
		return false
	}
	if end := start + n; end < len(s) && !nameBoundary(s[end]) {
		return false
	}
	return true
}

func nameBoundary(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return false
	case c == '.', c == '/', c == '-', c == '_':
		return false
	}
	return true
}
