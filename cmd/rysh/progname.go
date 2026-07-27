package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The binary ships under two names: `rysh` for the open-source build and `ry`
// for the closed one. Usage text is written with the literal "rysh"; the
// helpers here swap in whatever the binary was actually invoked as, so the `ry`
// build does not instruct people to type a command they do not have.

// progName reports the name this binary was invoked as, falling back to "rysh"
// when argv[0] is unusable. A "*.test" name (the binary `go test` builds) also
// falls back, so test output does not depend on the harness.
func progName() string {
	base := filepath.Base(os.Args[0])
	base = strings.TrimSuffix(base, ".exe")
	if base == "" || base == "." || base == string(filepath.Separator) ||
		strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".test") {
		return "rysh"
	}
	return base
}

// rewriteProgName replaces the standalone word "rysh" with the invoked binary
// name. Only the bare command word is replaced — anything touching '.', '/',
// '-' or '_' is a filename, path or identifier that keeps its real spelling:
// rysh.config.yaml, ./.rysh/, /tmp/rysh/logs/, rysh.lock.
func rewriteProgName(s string) string {
	name := progName()
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
		if standalone(s, j, len(word)) {
			b.WriteString(name)
		} else {
			b.WriteString(word)
		}
		i = j + len(word)
	}
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

// usageLine prints one line of usage text with the program name substituted.
func usageLine(s string) {
	fmt.Println(rewriteProgName(s))
}
