// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux

package session

// processArgs is not read on this platform; a record without PIDStart cannot
// be matched by argv here.
func processArgs(pid int) ([]string, bool) { return nil, false }
