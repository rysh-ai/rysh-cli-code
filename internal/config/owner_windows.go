// SPDX-License-Identifier: Apache-2.0

//go:build windows

package config

import "io/fs"

func ownedByMe(fs.FileInfo) bool { return true }
