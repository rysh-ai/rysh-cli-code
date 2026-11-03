// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package actors

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// ttyLastInput reports when the controlling terminal of pid last delivered
// input: the tty's access time, which the kernel bumps when a keystroke is read
// from it — the same clock `w` reads for its IDLE column. Output moves the
// modification time instead, so a terminal UI that only redraws stays idle.
//
// macOS has no /proc, so the terminal comes from the kernel's process table
// (kinfo_proc e_tdev). A pseudo-terminal there is /dev/ttysNNN with NNN its
// minor number; the device is stat'ed and its rdev checked against e_tdev, so a
// process without a terminal, or one whose device is not a ttys, reports
// nothing rather than another terminal's time.
func ttyLastInput(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || kp.Eproc.Tdev == -1 { // -1 is NODEV
		return time.Time{}, false
	}
	dev := uint64(uint32(kp.Eproc.Tdev))
	var st unix.Stat_t
	if err := unix.Stat(fmt.Sprintf("/dev/ttys%03d", unix.Minor(dev)), &st); err != nil {
		return time.Time{}, false
	}
	if uint64(uint32(st.Rdev)) != dev {
		return time.Time{}, false
	}
	return time.Unix(st.Atim.Unix()), true
}
