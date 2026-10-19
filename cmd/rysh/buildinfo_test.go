// SPDX-License-Identifier: Apache-2.0

package main

import (
	"runtime/debug"
	"testing"
)

func vcs(rev, modified string) []debug.BuildSetting {
	var s []debug.BuildSetting
	if rev != "" {
		s = append(s, debug.BuildSetting{Key: "vcs.revision", Value: rev})
	}
	if modified != "" {
		s = append(s, debug.BuildSetting{Key: "vcs.modified", Value: modified})
	}
	// vcs.time is present in real local builds and must never be used.
	return append(s, debug.BuildSetting{Key: "vcs.time", Value: "2026-09-23T00:00:00Z"})
}

func TestStampFromBuildInfo(t *testing.T) {
	const sha = "3222c1a18db8f0b15835bcd5672bd1b3e325f1ab"
	cases := []struct {
		name                string
		bi                  *debug.BuildInfo
		version, commit     string // what -ldflags left
		wantVersion, wantCo string
	}{
		{
			// goreleaser sets both; build info must not override a single value.
			name:    "goreleaser ldflags are left untouched",
			bi:      &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}, Settings: vcs("ffff", "true")},
			version: "0.2.9", commit: "4806c47",
			wantVersion: "0.2.9", wantCo: "4806c47",
		},
		{
			// `go install …@v0.2.9` via the module proxy: a version, no VCS data.
			name:    "go install at a tag gets the version only",
			bi:      &debug.BuildInfo{Main: debug.Module{Version: "v0.2.9"}},
			version: defaultVersion, commit: defaultCommit,
			wantVersion: "0.2.9", wantCo: defaultCommit,
		},
		{
			name:    "local build at a clean tag",
			bi:      &debug.BuildInfo{Main: debug.Module{Version: "v0.2.9"}, Settings: vcs(sha, "false")},
			version: defaultVersion, commit: defaultCommit,
			wantVersion: "0.2.9", wantCo: sha,
		},
		{
			name:    "local build with a dirty tree",
			bi:      &debug.BuildInfo{Main: debug.Module{Version: "v0.2.9+dirty"}, Settings: vcs(sha, "true")},
			version: defaultVersion, commit: defaultCommit,
			wantVersion: "0.2.9+dirty", wantCo: sha + "-dirty",
		},
		{
			// Measured, not hypothetical: go ignores a worktree's .git FILE, and a
			// worktree under the monorepo's worktrees/ is stamped with the ROOT
			// repo's HEAD, dirty — a commit that is not in rysh-cli at all. Go
			// leaves Main.Version at (devel) exactly when the repo it found is
			// not the module's own, so the revision is not trusted then either.
			name:    "(devel) means the revision may be another repo's; leave commit alone",
			bi:      &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs(sha, "true")},
			version: defaultVersion, commit: defaultCommit,
			wantVersion: defaultVersion, wantCo: defaultCommit,
		},
		{
			// -buildvcs=false, or a build outside any repository.
			name:    "no build info at all changes nothing",
			bi:      nil,
			version: defaultVersion, commit: defaultCommit,
			wantVersion: defaultVersion, wantCo: defaultCommit,
		},
		{
			// Each value is filled independently: only what -ldflags left alone.
			name:    "only the value ldflags left at its default is filled",
			bi:      &debug.BuildInfo{Main: debug.Module{Version: "v0.2.9"}, Settings: vcs(sha, "false")},
			version: "0.3.0", commit: defaultCommit,
			wantVersion: "0.3.0", wantCo: sha,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, co := stampFromBuildInfo(c.bi, c.version, c.commit)
			if v != c.wantVersion || co != c.wantCo {
				t.Errorf("got (%q, %q), want (%q, %q)", v, co, c.wantVersion, c.wantCo)
			}
		})
	}
}
