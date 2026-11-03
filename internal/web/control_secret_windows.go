// SPDX-License-Identifier: Apache-2.0

//go:build windows

package web

import "os"

// privateFileOK on Windows: there are no POSIX mode bits to check. The file
// lives in the user's own profile, whose ACL already limits it to the user;
// the read-once-and-delete rule still applies.
func privateFileOK(fi os.FileInfo) error { return nil }
