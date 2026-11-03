// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package actors

import "time"

// ttyLastInput has no implementation on this platform, so every terminal UI
// counts as in use and the smallest claim wins, as it did before input time
// was considered (see idleViewportClaims).
func ttyLastInput(int) (time.Time, bool) { return time.Time{}, false }
