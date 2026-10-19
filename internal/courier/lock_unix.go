// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package courier

import (
	"os"
	"path/filepath"
	"syscall"
)

// LockPane takes the per-pane listener lock, so one agent has exactly one
// listener. ok=false means another listener holds it. The returned file must
// stay open for as long as the lock is wanted; the OS drops it on exit.
func LockPane(path string) (f *os.File, ok bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false, nil
	}
	return f, true, nil
}
