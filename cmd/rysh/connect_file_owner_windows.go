// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import "os"

// checkConnectFileOwner: Windows has no unix mode bits or uid on FileInfo; the
// file stays in the user's own profile folders.
func checkConnectFileOwner(os.FileInfo) error { return nil }
