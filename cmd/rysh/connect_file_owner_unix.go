// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// checkConnectFileOwner refuses a connect file another local user could have
// rewritten to name another server: group/other-writable, or owned by someone
// else. A browser download (0644) passes; the file holds only a one-time code.
func checkConnectFileOwner(st os.FileInfo) error {
	if st.Mode().Perm()&0o022 != 0 {
		return errors.New("other users can write it; download it again from the company page")
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok && int(s.Uid) != os.Getuid() {
		return errors.New("it belongs to another user")
	}
	return nil
}
