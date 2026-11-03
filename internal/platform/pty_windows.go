// SPDX-License-Identifier: Apache-2.0

//go:build windows

// Package platform reports host capabilities that change what rysh can do,
// so the answer is a compile-time constant rather than a runtime surprise.
package platform

// PTYSupported is true on native Windows: panes run on a pseudo-console
// (ConPTY, internal/conpty) instead of creack/pty, whose Windows build is a
// stub. ConPTY needs Windows 10 1809 or later; on an older Windows the pane
// says so when its shell fails to start.
//
// Native Windows panes are built but have NOT been tested on a real Windows
// host (founder decision, root-order-100 §4: no final Windows test for this
// release). WSL2 remains the tested path; see docs/windows.md.
const PTYSupported = true

// PTYUnsupportedReason explains, in user-facing terms, why panes cannot run.
// Empty when PTYSupported is true.
const PTYUnsupportedReason = ""
