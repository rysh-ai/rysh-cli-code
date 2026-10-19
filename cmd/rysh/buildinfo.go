// SPDX-License-Identifier: Apache-2.0

package main

import (
	"runtime/debug"
	"strings"
)

// The values version and commit hold when -ldflags did not set them.
const (
	defaultVersion = "dev"
	defaultCommit  = "none"
)

// stampFromBuildInfo fills version and commit from the Go toolchain's build info,
// but only where -ldflags left them at their defaults. goreleaser sets both, so a
// release binary is never touched; `go install` and plain `go build` set neither,
// and used to report "dev (commit: none)" even for a tagged module.
//
//   - version <- Main.Version without its "v", matching goreleaser's {{.Version}}.
//     A module-proxy build (`go install …@v0.2.9`) has this and nothing else.
//   - commit  <- vcs.revision, suffixed "-dirty" when vcs.modified is true. Only
//     a build from a checkout has it; a proxy build carries no VCS data at all.
//     It is trusted ONLY when Main.Version is a real version. Go ignores a git
//     worktree's .git file and keeps walking up, so a worktree nested in another
//     repository — every worktree under the monorepo's worktrees/ — is stamped
//     with the OUTER repo's HEAD, marked dirty: a commit that is not in this
//     repository at all. Go leaves Main.Version at "(devel)" in exactly that
//     case, because the repository it found is not the module's own.
//
// date is deliberately NOT filled: the only candidate, vcs.time, is the commit
// time, and printing it after "built:" would claim a build time nobody measured.
func stampFromBuildInfo(bi *debug.BuildInfo, version, commit string) (string, string) {
	if bi == nil {
		return version, commit
	}
	v := bi.Main.Version
	if v == "" || v == "(devel)" {
		return version, commit
	}
	if version == defaultVersion {
		version = strings.TrimPrefix(v, "v")
	}
	if commit == defaultCommit {
		var rev string
		var dirty bool
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			commit = rev
			if dirty {
				commit += "-dirty"
			}
		}
	}
	return version, commit
}

// applyBuildInfo runs stampFromBuildInfo against the running binary's build info.
func applyBuildInfo() {
	bi, _ := debug.ReadBuildInfo() // nil when the binary carries none
	version, commit = stampFromBuildInfo(bi, version, commit)
}
