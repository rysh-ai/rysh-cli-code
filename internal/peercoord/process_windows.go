// SPDX-License-Identifier: Apache-2.0

package peercoord

import (
	"fmt"
	"os"
	"os/exec"
)

func lockFile(*os.File) error {
	return fmt.Errorf("managed peer agents require a Unix PTY host; use WSL on Windows")
}
func unlockFile(*os.File) {}
func checkSocketDirectory(os.FileInfo) error {
	return fmt.Errorf("managed peer agents require a Unix PTY host")
}
func configureProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
