// SPDX-License-Identifier: Apache-2.0

package cdp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A browser that dies before printing its DevTools line is a launch failure
// now, not a wait for the whole timeout: the wait hid the real error and
// burned the caller's retry budget.
func TestLaunchFailsFastWhenChromiumExits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	stub := filepath.Join(t.TempDir(), "dies-at-once")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'cannot start' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	b, err := Launch(context.Background(), LaunchOptions{BinPath: stub, UserDataDir: t.TempDir(), WaitTimeout: 30 * time.Second})
	if err == nil {
		b.Close()
		t.Fatal("launch of a dead browser succeeded")
	}
	if !strings.Contains(err.Error(), "exited before") {
		t.Fatalf("err = %v, want the early-exit error", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("early exit took %s to report; it waited out the timeout", d.Round(time.Millisecond))
	}
}
