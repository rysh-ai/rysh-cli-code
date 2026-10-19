// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// resolveBoardSession finds the session a `rysh board` command talks to.
//
// FOUND LIVE: every fleet worker started with `--worktrees` runs in
// `worktrees/<name>`, and from there `rysh board post|tail` failed with
// `session "datalake" not found` although $RYSH_SESSION was set. The name was
// right; the DIRECTORY was not. The session registry is cwd-derived
// (<cwd>/.rysh/sessions, internal/config), and a worktree has no .rysh of its
// own — so board posting silently broke for the whole fleet.
//
// The order, and it is the task's order:
//
//  1. the name: --session, else $RYSH_SESSION, else the config's;
//  2. the registry the config resolved, if it holds that name;
//  3. otherwise WALK UP from the cwd to the first workspace whose registry
//     holds it — a subdirectory or a git worktree under the workspace finds
//     the workspace's session;
//  4. otherwise the configured registry, whose "not found" is then the truth.
//
// An explicit RYSH_DIR / RYSH_SESSION_DIR is obeyed and never walked past: an
// operator who named a registry meant that one.
func resolveBoardSession(cfg config.Config, explicit string) (*session.Store, string, error) {
	name := strings.TrimSpace(explicit)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("RYSH_SESSION"))
	}
	if name == "" {
		name = cfg.SessionName
	}
	if name == "" {
		name = "default"
	}

	pinned := strings.TrimSpace(os.Getenv("RYSH_DIR")) != "" ||
		strings.TrimSpace(os.Getenv("RYSH_SESSION_DIR")) != ""
	if !pinned && !sessionRecordExists(cfg.SessionDir, name) {
		if cwd, err := os.Getwd(); err == nil {
			if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
				cwd = resolved
			}
			if dir := findSessionDirUpward(cwd, name); dir != "" {
				cfg.SessionDir = dir
			}
		}
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return nil, "", err
	}
	return store, name, nil
}

// findSessionDirUpward walks from start to the filesystem root and returns the
// first session registry that holds a record for name, or "".
//
// At each level a workspace is recognised the way internal/config recognises
// one — a rysh.config.yaml beside a .rysh, or inside it — and that config's
// own session dir is honoured, so a workspace that relocated its registry
// (session.dir) is still found. A bare .rysh/sessions counts too: the default
// layout needs no config file at all.
func findSessionDirUpward(start, name string) string {
	for dir := start; ; {
		for _, cand := range sessionDirCandidates(dir) {
			if sessionRecordExists(cand, name) {
				return cand
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func sessionDirCandidates(dir string) []string {
	var out []string
	for _, cf := range []string{
		filepath.Join(dir, "rysh.config.yaml"),
		filepath.Join(dir, ".rysh", "rysh.config.yaml"),
	} {
		if info, err := os.Stat(cf); err == nil && info.Mode().IsRegular() {
			if sd := config.LoadFrom(cf).SessionDir; sd != "" {
				out = append(out, sd)
			}
			break
		}
	}
	return append(out, filepath.Join(dir, ".rysh", "sessions"))
}

// sessionRecordExists mirrors session.Store's file naming (pathFor) without
// creating the directory the way session.NewStore does — probing must not
// litter every ancestor with an empty .rysh/sessions.
func sessionRecordExists(dir, name string) bool {
	if dir == "" {
		return false
	}
	file := strings.ReplaceAll(strings.TrimSpace(name), string(filepath.Separator), "-") + ".json"
	info, err := os.Stat(filepath.Join(dir, file))
	return err == nil && info.Mode().IsRegular()
}
