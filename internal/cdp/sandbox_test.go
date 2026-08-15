// SPDX-License-Identifier: Apache-2.0

package cdp

import (
	"strings"
	"testing"
)

func has(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// The decision matrix is the security-relevant part of this change, so it is
// pinned rather than left to a smoke test. --no-sandbox must appear ONLY where
// the sandbox provably cannot work or the operator asked for it.
func TestSandboxArgsDecisionMatrix(t *testing.T) {
	const bigShm = 1 << 30 // 1 GiB — a normal host

	cases := []struct {
		name         string
		env          sandboxEnv
		wantNoSbx    bool
		wantDevShm   bool
		wantReasoned bool // a non-empty reason must accompany a disabled sandbox
	}{
		{
			name: "normal linux user keeps the sandbox",
			env:  sandboxEnv{GOOS: "linux", UID: 1000, ShmBytes: bigShm},
		},
		{
			name: "macOS keeps the sandbox and never gets the shm flag",
			env:  sandboxEnv{GOOS: "darwin", UID: 501},
		},
		{
			// The container/CI case this change exists for.
			name:         "root on linux disables the sandbox, with a reason",
			env:          sandboxEnv{GOOS: "linux", UID: 0, ShmBytes: bigShm},
			wantNoSbx:    true,
			wantReasoned: true,
		},
		{
			name:       "small /dev/shm gets the resource flag but keeps the sandbox",
			env:        sandboxEnv{GOOS: "linux", UID: 1000, ShmBytes: 64 << 20},
			wantDevShm: true,
		},
		{
			name:         "explicit opt-in disables it even as a normal user",
			env:          sandboxEnv{GOOS: "linux", UID: 1000, ShmBytes: bigShm, NoSandbox: "1"},
			wantNoSbx:    true,
			wantReasoned: true,
		},
		{
			// Someone who would rather the launch FAIL than run unsandboxed.
			// This must beat the root heuristic, or the escape hatch is a lie.
			name: "forced sandbox wins over the root heuristic",
			env:  sandboxEnv{GOOS: "linux", UID: 0, ShmBytes: bigShm, ForceSbx: "1"},
		},
		{
			name: "forced sandbox wins over an explicit opt-in too",
			env:  sandboxEnv{GOOS: "linux", UID: 0, ShmBytes: bigShm, NoSandbox: "1", ForceSbx: "true"},
		},
		{
			name:       "root in a container gets both flags",
			env:        sandboxEnv{GOOS: "linux", UID: 0, ShmBytes: 64 << 20},
			wantNoSbx:  true,
			wantDevShm: true,
		},
		{
			// Measured on a GitHub runner: uid 1001, roomy /dev/shm, but
			// apparmor_restrict_unprivileged_userns=1. Chromium: "No usable
			// sandbox!". This is also stock Ubuntu 23.10+, so it is a real
			// user configuration, not a CI quirk.
			name:         "AppArmor userns restriction disables the sandbox for a non-root user",
			env:          sandboxEnv{GOOS: "linux", UID: 1001, ShmBytes: bigShm, ApparmorUsernsRestricted: true},
			wantNoSbx:    true,
			wantReasoned: true,
		},
		{
			// The forced hatch must beat this cause too, or it is not a hatch.
			name: "forced sandbox wins over the AppArmor restriction",
			env:  sandboxEnv{GOOS: "linux", UID: 1001, ShmBytes: bigShm, ApparmorUsernsRestricted: true, ForceSbx: "1"},
		},
		{
			// The flag is Linux-shaped; it must never leak to macOS.
			name: "AppArmor field is ignored on darwin",
			env:  sandboxEnv{GOOS: "darwin", UID: 501, ApparmorUsernsRestricted: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, reason := sandboxArgs(tc.env)

			if got := has(args, "--no-sandbox"); got != tc.wantNoSbx {
				t.Errorf("--no-sandbox present = %v, want %v (args=%v)", got, tc.wantNoSbx, args)
			}
			if got := has(args, "--disable-dev-shm-usage"); got != tc.wantDevShm {
				t.Errorf("--disable-dev-shm-usage present = %v, want %v (args=%v)", got, tc.wantDevShm, args)
			}
			// A disabled sandbox must always carry a reason, because the caller
			// logs it. Silence here would mean an unsandboxed browser with no
			// trace of why.
			if tc.wantReasoned && strings.TrimSpace(reason) == "" {
				t.Error("sandbox disabled without a reason — nothing would be logged")
			}
			if !tc.wantNoSbx && reason != "" {
				t.Errorf("sandbox kept but a disable-reason was returned: %q", reason)
			}
		})
	}
}

// The shm flag is a resource workaround, not a security decision, and must not
// drag the sandbox down with it.
func TestSmallShmDoesNotDisableSandbox(t *testing.T) {
	args, reason := sandboxArgs(sandboxEnv{GOOS: "linux", UID: 1000, ShmBytes: 1 << 20})
	if has(args, "--no-sandbox") {
		t.Fatal("a small /dev/shm must not disable the sandbox — it is unrelated")
	}
	if reason != "" {
		t.Fatalf("unexpected disable reason: %q", reason)
	}
}

// Headed launches are real desktop sessions: both problems are absent and
// dropping the sandbox there would be indefensible.
func TestHeadedLaunchNeverCarriesSandboxFlags(t *testing.T) {
	args := strings.Join(launchArgs(LaunchOptions{UserDataDir: "/tmp/x"}), " ")
	if strings.Contains(args, "--no-sandbox") {
		t.Fatalf("headed launch must never pass --no-sandbox: %s", args)
	}
	if strings.Contains(args, "--disable-dev-shm-usage") {
		t.Fatalf("headed launch must not pass --disable-dev-shm-usage: %s", args)
	}
}

func TestTruthy(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " yes ", "on"} {
		if !isTruthy(v) {
			t.Errorf("isTruthy(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "maybe"} {
		if isTruthy(v) {
			t.Errorf("isTruthy(%q) = true, want false", v)
		}
	}
}
