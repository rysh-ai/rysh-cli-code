// SPDX-License-Identifier: Apache-2.0

//go:build windows

package courier

import (
	"os"
	"path/filepath"
)

// LockPane on Windows only creates the lock file: there is no advisory flock,
// and fleets (which start listeners) run on macOS and Linux.
func LockPane(path string) (*os.File, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	return f, err == nil, err
}
