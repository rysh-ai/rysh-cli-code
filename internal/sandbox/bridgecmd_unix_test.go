// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package sandbox

import "testing"

// The proxy bridge must die with its sandbox's process group, so on Unix it
// starts in its own group (Setpgid). The attribute sits behind a build tag
// because syscall.SysProcAttr has no Setpgid on Windows, where naming it broke
// the Windows cross-build of everything that imports sandbox (gap01 67c6623).
func TestBridgeProcAttrPutsTheBridgeInItsOwnGroup(t *testing.T) {
	a := bridgeProcAttr()
	if a == nil || !a.Setpgid {
		t.Fatalf("bridgeProcAttr() = %+v, want Setpgid: true", a)
	}
}
