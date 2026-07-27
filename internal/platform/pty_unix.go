//go:build !windows

// Package platform reports host capabilities that change what rysh can do,
// so the answer is a compile-time constant rather than a runtime surprise.
package platform

// PTYSupported reports whether this build can allocate a pseudo-terminal.
//
// Every pane is a PTY-backed shell, so this gates the core of the product.
const PTYSupported = true

// PTYUnsupportedReason explains, in user-facing terms, why panes cannot run.
// Empty when PTYSupported is true.
const PTYUnsupportedReason = ""
