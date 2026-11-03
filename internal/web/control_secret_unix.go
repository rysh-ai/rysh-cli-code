// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package web

import (
	"errors"
	"os"
	"syscall"
)

// privateFileOK checks that a handover file is owned by this user and has no
// group or other permission bits.
func privateFileOK(fi os.FileInfo) error {
	if fi.Mode().Perm()&0o077 != 0 {
		return errors.New("readable or writable by others (want mode 0600)")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read the file's owner")
	}
	if int(st.Uid) != os.Getuid() {
		return errors.New("not owned by this user")
	}
	return nil
}
