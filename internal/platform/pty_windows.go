// SPDX-License-Identifier: Apache-2.0

//go:build windows

// Package platform reports host capabilities that change what rysh can do,
// so the answer is a compile-time constant rather than a runtime surprise.
package platform

// PTYSupported is false on native Windows.
//
// rysh's PTY layer is github.com/creack/pty, whose Windows build is a stub:
// it compiles, so a windows/amd64 binary links and runs, and then every
// pty.Start fails at the moment the user tries to open their first pane.
// Native support needs a ConPTY implementation behind this same seam — design
// 011 gates that on demonstrated demand. Until then the supported Windows path
// is WSL2, where the Linux binary runs unmodified.
const PTYSupported = false

// PTYUnsupportedReason explains, in user-facing terms, why panes cannot run.
const PTYUnsupportedReason = "native Windows has no PTY support in this build " +
	"(rysh needs ConPTY, which is not implemented yet)"
