// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package actors

// kernelName is what processName reports for a process exec'd through exe.
// darwin names the process after the binary the path resolves to, which the
// caller passes as darwinName: a symlink named claude to versions/2.1.296 is
// "2.1.296".
func kernelName(exe, darwinName string) string { return darwinName }
