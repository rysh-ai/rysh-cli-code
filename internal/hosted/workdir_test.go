// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/remote"
)

// QA-15 (2026-11-01): WorkDir was the absolute "/home/rysh/work". The release
// binary path guard (scripts/release/check-binary-paths.sh, also a goreleaser
// post hook) reads any /home/<name>/ in a binary as a builder's home, so every
// cross-compiled rysh failed it. The work directory is the SSH user's home's
// `work`, and `ssh host 'cd work && …'` starts in that home, so a relative
// WorkDir names the same directory and puts no /home/ path in the binary.
func TestWorkDirIsRelativeToTheRunnerUsersHome(t *testing.T) {
	if filepath.IsAbs(WorkDir) || strings.Contains(WorkDir, "/home/") || WorkDir == "" {
		t.Fatalf("WorkDir = %q: want a path relative to %s's home, with no /home/ in it", WorkDir, SSHUser)
	}
	args, err := remote.SSHArgs(remote.SSHOptions{Host: "hosted-0123456789abcdef", Session: SessionName, RemoteDir: WorkDir})
	if err != nil {
		t.Fatal(err)
	}
	if cmd := args[len(args)-1]; !strings.HasPrefix(cmd, "cd 'work' && exec ") {
		t.Errorf("remote command = %q, want it to cd into work first", cmd)
	}
}
