// SPDX-License-Identifier: Apache-2.0

//go:build windows

package session

import (
	"errors"
	"syscall"
)

// wsaeConnRefused is Winsock's WSAECONNREFUSED, which a refused connect
// returns on Windows instead of syscall.ECONNREFUSED.
const wsaeConnRefused = syscall.Errno(10061)

// isConnRefused reports whether err is a TCP connect refused because nothing
// listens on the port.
func isConnRefused(err error) bool {
	return errors.Is(err, wsaeConnRefused) || errors.Is(err, syscall.ECONNREFUSED)
}
