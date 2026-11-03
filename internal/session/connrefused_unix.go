// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package session

import (
	"errors"
	"syscall"
)

// isConnRefused reports whether err is a TCP connect refused because nothing
// listens on the port.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
