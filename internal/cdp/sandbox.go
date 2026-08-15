// SPDX-License-Identifier: Apache-2.0

package cdp

// Chromium sandbox handling for containerised environments.
//
// THE PROBLEM
//
// `--headless=new` alone does not start in a container. Two independent causes,
// which are worth keeping separate because only one of them is a security
// decision:
//
//  1. /dev/shm defaults to 64 MiB under Docker. Chromium puts its shared memory
//     there, exhausts it, and the renderer dies. `--disable-dev-shm-usage` moves
//     that to /tmp. No security implication whatsoever — it is a resource fix.
//
//  2. Chromium's sandbox needs unprivileged user namespaces, and cannot get
//     them in two distinct situations:
//
//       a. running as root — the setuid sandbox refuses outright;
//       b. AppArmor restricting unprivileged userns, which is the DEFAULT on
//          Ubuntu 23.10+. Chromium says so itself: "No usable sandbox! If you
//          are running on Ubuntu 23.10+ ... that has disabled unprivileged user
//          namespaces with AppArmor".
//
//     (b) is not an exotic CI condition — it is stock modern Ubuntu, so without
//     this detection rysh's browser automation is broken for those users
//     outright. It was measured on a GitHub runner (uid 1001, NOT root, with
//     apparmor_restrict_unprivileged_userns=1), which is why the root-only
//     version of this heuristic did not help there.
//
// WHY THIS IS NOT JUST "ADD --no-sandbox"
//
// The sandbox is the boundary between a compromised renderer and the host. rysh
// drives this browser at agent direction, across pages rysh did not choose, so
// that boundary is load-bearing: `browser_action` visiting a hostile page with
// the sandbox off is arbitrary code execution against the machine running the
// agent. Passing --no-sandbox unconditionally would trade a real security
// property for the convenience of one environment.
//
// So it is disabled only where it provably cannot work — running as root on
// Linux — or where the operator explicitly asks. And when it is disabled, that
// is logged, because a silently unsandboxed browser is worse than a loud one.
//
// Escape hatches, both directions:
//
//	RYSH_BROWSER_NO_SANDBOX=1   disable it even when we would not have
//	RYSH_BROWSER_SANDBOX=1      keep it even as root — the launch will likely
//	                            fail, which is the correct outcome for someone
//	                            who would rather not run unsandboxed at all

import (
	"log/slog"
	"os"
	"runtime"
	"strings"
)

// sandboxEnv is the environment probe, injectable so the decision logic is
// testable without a container.
type sandboxEnv struct {
	GOOS      string
	UID       int
	ShmBytes  uint64 // 0 = unknown / not applicable
	NoSandbox string // RYSH_BROWSER_NO_SANDBOX
	ForceSbx  string // RYSH_BROWSER_SANDBOX
	// ApparmorUsernsRestricted mirrors
	// /proc/sys/kernel/apparmor_restrict_unprivileged_userns == 1: Ubuntu
	// 23.10+ blocks the namespaces Chromium's sandbox is built on.
	ApparmorUsernsRestricted bool
}

// smallShmThreshold is Docker's default /dev/shm size. At or below it,
// Chromium's renderer reliably dies partway through a page load.
const smallShmThreshold = 64 << 20 // 64 MiB

// sandboxArgs decides the sandbox-related flags. Returns the flags plus a
// reason when the sandbox was turned off (empty when it was left on), so the
// caller can log exactly once rather than on every helper call.
func sandboxArgs(e sandboxEnv) (args []string, disabledReason string) {
	// /dev/shm — a resource fix, decided independently of the sandbox.
	if e.GOOS == "linux" && e.ShmBytes > 0 && e.ShmBytes <= smallShmThreshold {
		args = append(args, "--disable-dev-shm-usage")
	}

	forced := isTruthy(e.ForceSbx)
	asked := isTruthy(e.NoSandbox)

	switch {
	case forced:
		// Explicit "keep the sandbox" wins over every heuristic, including
		// running as root. The launch may fail; that is the point.
		return args, ""
	case asked:
		args = append(args, "--no-sandbox")
		return args, "RYSH_BROWSER_NO_SANDBOX=1 was set"
	case e.GOOS == "linux" && e.UID == 0:
		// Chromium's setuid sandbox refuses to run as root — the container case.
		args = append(args, "--no-sandbox")
		return args, "running as root on Linux, where Chromium's setuid sandbox refuses to start"
	case e.GOOS == "linux" && e.ApparmorUsernsRestricted:
		// Stock Ubuntu 23.10+. Chromium reports "No usable sandbox!" and dies.
		args = append(args, "--no-sandbox")
		return args, "AppArmor restricts unprivileged user namespaces (the default on Ubuntu 23.10+), so Chromium reports \"No usable sandbox\" and refuses to start"
	default:
		return args, ""
	}
}

// probeSandboxEnv reads the real environment.
func probeSandboxEnv() sandboxEnv {
	return sandboxEnv{
		GOOS:      runtime.GOOS,
		UID:       os.Getuid(),
		ShmBytes:  shmSize(),
		NoSandbox: os.Getenv("RYSH_BROWSER_NO_SANDBOX"),
		ForceSbx:  os.Getenv("RYSH_BROWSER_SANDBOX"),

		ApparmorUsernsRestricted: apparmorUsernsRestricted(),
	}
}

// apparmorUsernsRestricted reports whether AppArmor is blocking unprivileged
// user namespaces. A missing file means not restricted (older kernels,
// non-Ubuntu distros, macOS).
func apparmorUsernsRestricted() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	b, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "1"
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// warnSandboxDisabled states the trade-off plainly. A browser running without
// its sandbox should never be a silent condition.
func warnSandboxDisabled(reason string) {
	if reason == "" {
		return
	}
	slog.Warn("browser sandbox DISABLED — a compromised page can escape to this host",
		"reason", reason,
		"mitigation", "run rysh as a non-root user, or set RYSH_BROWSER_SANDBOX=1 to refuse to launch unsandboxed")
}
