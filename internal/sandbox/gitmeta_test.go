// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LEAD-CHANGES (2): git-dir redirection files and submodule git dirs are held
// like the top-level metadata. Each of them can point the user's next
// host-side git command at code the agent wrote.

// submoduleLayout builds, by hand and offline, what `git submodule add` leaves:
// .git/modules/sub (a git dir whose core.worktree is ../../../sub), the
// working tree's sub/.git file pointing at it, and a nested
// .git/modules/sub/modules/inner for sub/inner.
func submoduleLayout(t *testing.T, ws string) (git, sub, inner string) {
	t.Helper()
	git = filepath.Join(ws, ".git")
	writeFile(t, filepath.Join(git, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(git, "config"), "[core]\n")
	sub = filepath.Join(git, "modules", "sub")
	writeFile(t, filepath.Join(sub, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(sub, "config"), "[core]\n\tworktree = ../../../sub\n")
	writeFile(t, filepath.Join(ws, "sub", ".git"), "gitdir: ../.git/modules/sub\n")
	inner = filepath.Join(sub, "modules", "inner")
	writeFile(t, filepath.Join(inner, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(inner, "config"), "[core]\n\tworktree = ../../../../../sub/inner\n")
	writeFile(t, filepath.Join(ws, "sub", "inner", ".git"), "gitdir: ../../.git/modules/sub/modules/inner\n")
	return git, sub, inner
}

// mustList unwraps a ([]string, error) result, failing the test on error.
func mustList(t *testing.T) func([]string, error) []string {
	return func(l []string, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
}

func has(list []string, want string) bool {
	for _, x := range list {
		if x == want {
			return true
		}
	}
	return false
}

func TestGitMetaCoversRedirectionFilesAndSubmodules(t *testing.T) {
	l := newLayout(t)
	git, sub, inner := submoduleLayout(t, l.ws)
	meta := mustList(t)(GitMetaPaths(l.ws, ""))
	for _, want := range []string{
		filepath.Join(git, "commondir"), filepath.Join(git, "gitdir"),
		filepath.Join(sub, "config"), filepath.Join(sub, "hooks"), filepath.Join(sub, "commondir"),
		filepath.Join(inner, "config"), filepath.Join(inner, "hooks"),
	} {
		if !has(meta, want) {
			t.Errorf("GitMetaPaths missing %s", want)
		}
	}
	pins := mustList(t)(GitPins(l.ws, ""))
	for _, want := range []string{sub, inner, filepath.Join(l.ws, "sub", ".git"), filepath.Join(l.ws, "sub", "inner", ".git")} {
		if !has(pins, want) {
			t.Errorf("GitPins missing %s", want)
		}
	}
}

func TestSubmoduleWorktreeOutsideWorkspaceIsNotPinned(t *testing.T) {
	l := newLayout(t)
	git := filepath.Join(l.ws, ".git")
	writeFile(t, filepath.Join(git, "HEAD"), "x")
	m := filepath.Join(git, "modules", "evil")
	writeFile(t, filepath.Join(m, "HEAD"), "x")
	writeFile(t, filepath.Join(m, "config"), "[core]\n\tworktree = ../../../../../../etc\n")
	for _, p := range mustList(t)(GitPins(l.ws, "")) {
		if !within(p, l.ws) && !within(p, git) {
			t.Errorf("a pin escaped the workspace: %s", p)
		}
	}
}

func TestMacOSHoldsSubmodulesAndRedirection(t *testing.T) {
	l := newLayout(t)
	_, sub, _ := submoduleLayout(t, l.ws)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`(deny file-write* (literal "` + filepath.Join(l.ws, ".git", "commondir") + `"))`,
		`(deny file-write* (subpath "` + filepath.Join(sub, "hooks") + `"))`,
		`(deny file-write* (literal "` + filepath.Join(l.ws, "sub", ".git") + `"))`,
		// Module git dirs created later are held by pattern.
		`(deny file-write* (regex #"^` + regexQuoteSBPL(filepath.Join(l.ws, ".git", "modules")) + `/`,
		// A new .git anywhere under the workspace, and nested repos' metadata
		// (round 3, 2b).
		`(deny file-write* (regex #"^` + regexQuoteSBPL(l.ws) + `/(.+/)?\.git$"))`,
		`(deny file-write* (regex #"^` + regexQuoteSBPL(l.ws) + `/(.+/)?\.git/(hooks(/.*)?|config|config\.worktree|commondir|gitdir)$"))`,
	} {
		if !strings.Contains(plan.SBPL, want) {
			t.Errorf("profile missing %q", want)
		}
	}
}

func TestContainerHoldsSubmodulesAndGitFilesReadOnly(t *testing.T) {
	l := newLayout(t)
	_, sub, _ := submoduleLayout(t, l.ws)
	plan := containerPlan(t, l, nil)
	args := strings.Join(plan.Argv, " ")
	for _, p := range []string{
		filepath.Join(sub, "config"),
		filepath.Join(sub, "hooks"),
		filepath.Join(l.ws, "sub", ".git"), // a pointer file: READ-ONLY, not just pinned
	} {
		if !strings.Contains(args, "type=bind,source="+p+",target="+p+",readonly") {
			t.Errorf("not bound read-only: %s", p)
		}
	}
	if !strings.Contains(args, "type=bind,source="+sub+",target="+sub+" ") {
		t.Errorf("module git dir not pinned: %s", sub)
	}
	if _, err := os.Stat(filepath.Join(sub, "commondir")); err == nil {
		t.Error("commondir created on the host (an empty commondir breaks git)")
	}
}

// A linked worktree's .git FILE is a pointer: bound read-only, so the agent
// cannot rewrite where it points.
func TestContainerLinkedWorktreeGitFileReadOnly(t *testing.T) {
	l := newLayout(t)
	common := filepath.Join(l.root, "main", ".git")
	wtGit := filepath.Join(common, "worktrees", "proj")
	writeFile(t, filepath.Join(wtGit, "HEAD"), "x")
	writeFile(t, filepath.Join(wtGit, "commondir"), "../..\n")
	writeFile(t, filepath.Join(common, "config"), "[core]\n")
	writeFile(t, filepath.Join(l.ws, ".git"), "gitdir: "+wtGit+"\n")
	plan := containerPlan(t, l, func(_ *Profile, s *Spec) { s.GitCommonDir = common })
	args := strings.Join(plan.Argv, " ")
	for _, p := range []string{filepath.Join(l.ws, ".git"), filepath.Join(wtGit, "commondir")} {
		if !strings.Contains(args, "type=bind,source="+p+",target="+p+",readonly") {
			t.Errorf("not bound read-only: %s", p)
		}
	}
}

// A module git dir whose HEAD an earlier run deleted (its dir is writable;
// only the metadata is held) must still be found: its config, which no
// sandbox can delete, marks it. Found by the real-docker escape run: losing
// it un-held the submodule's pointer file on the next launch.
func TestModuleGitDirStillHeldWithoutHEAD(t *testing.T) {
	l := newLayout(t)
	_, sub, _ := submoduleLayout(t, l.ws)
	if err := os.Remove(filepath.Join(sub, "HEAD")); err != nil {
		t.Fatal(err)
	}
	pins := mustList(t)(GitPins(l.ws, ""))
	if !has(pins, sub) || !has(pins, filepath.Join(l.ws, "sub", ".git")) {
		t.Fatalf("module without HEAD dropped from pins: %v", pins)
	}
}

// LEAD-CHANGES round 3 (2a): the submodule walk is bounded, and it must never
// stop SILENTLY. An agent on container/bwrap can create hundreds of decoy
// module git dirs that sort before the real one; a walk that quietly stopped
// at its cap would leave the real submodule's hooks and config writable on the
// next launch. Every backend refuses the launch instead.
func TestModuleWalkCapRefusesLaunch(t *testing.T) {
	cases := map[string]func(t *testing.T, ws string){
		"too many module git dirs": func(t *testing.T, ws string) {
			for i := 0; i < maxModuleDirs+44; i++ {
				writeFile(t, filepath.Join(ws, ".git", "modules", fmt.Sprintf("aa%04d", i), "HEAD"), "x")
			}
		},
		"modules nested too deep": func(t *testing.T, ws string) {
			p := filepath.Join(ws, ".git", "modules")
			for i := 0; i < 12; i++ {
				p = filepath.Join(p, fmt.Sprintf("d%d", i))
			}
			writeFile(t, filepath.Join(p, "config"), "[core]\n")
		},
	}
	for name, decoys := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLayout(t)
			submoduleLayout(t, l.ws)
			decoys(t, l.ws)
			agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
			if _, err := macPlan(t, l, agent, nil); err == nil || !strings.Contains(err.Error(), "submodule") {
				t.Errorf("macOS: launch not refused: %v", err)
			}
			if _, err := bwrapPlan(t, l, func(p *Profile, _ *Spec) { p.Network = false }); err == nil || !strings.Contains(err.Error(), "submodule") {
				t.Errorf("bwrap: launch not refused: %v", err)
			}
			stdinIsTTY = func() bool { return true }
			t.Cleanup(func() { stdinIsTTY = realStdinIsTTY })
			p := builtinDefault()
			p.Backend, p.Image = BackendContainer, "local/agent:1"
			if _, err := (&Container{LookPath: fakeLook("docker"), UID: 501, GID: 20}).Prepare(l.spec(p)); err == nil || !strings.Contains(err.Error(), "submodule") {
				t.Errorf("container: launch not refused: %v", err)
			}
		})
	}
}

// Round 4: a directory under .git/modules that the walk cannot read (an agent
// ran chmod 000 on it in an earlier run) refuses the launch. Skipping it would
// hold nothing there; the agent restores the mode and writes the module's
// hooks or config. "not searchable" keeps the listing but breaks Lstat of the
// entries, the isGitDirMarker path.
func TestModuleWalkUnreadableDirRefusesLaunch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	cases := map[string]func(t *testing.T, git, sub, inner string) string{
		"modules mode 000":        func(t *testing.T, git, _, _ string) string { return filepath.Join(git, "modules") },
		"module git dir mode 000": func(t *testing.T, _, sub, _ string) string { return sub },
		"nested modules mode 000": func(t *testing.T, _, sub, _ string) string { return filepath.Join(sub, "modules") },
		"slash-named module mode 000": func(t *testing.T, git, _, _ string) string {
			d := filepath.Join(git, "modules", "a")
			writeFile(t, filepath.Join(d, "b", "HEAD"), "ref: refs/heads/main\n")
			return d
		},
	}
	modes := map[string]os.FileMode{"mode 000": 0, "not searchable": 0o644}
	for name, target := range cases {
		for mname, mode := range modes {
			t.Run(name+"/"+mname, func(t *testing.T) {
				l := newLayout(t)
				git, sub, inner := submoduleLayout(t, l.ws)
				dir := target(t, git, sub, inner)
				if err := os.Chmod(dir, mode); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
				agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
				if _, err := macPlan(t, l, agent, nil); err == nil || !strings.Contains(err.Error(), "submodule") {
					t.Errorf("macOS: launch not refused: %v", err)
				}
				if _, err := bwrapPlan(t, l, func(p *Profile, _ *Spec) { p.Network = false }); err == nil || !strings.Contains(err.Error(), "submodule") {
					t.Errorf("bwrap: launch not refused: %v", err)
				}
				stdinIsTTY = func() bool { return true }
				t.Cleanup(func() { stdinIsTTY = realStdinIsTTY })
				p := builtinDefault()
				p.Backend, p.Image = BackendContainer, "local/agent:1"
				if _, err := (&Container{LookPath: fakeLook("docker"), UID: 501, GID: 20}).Prepare(l.spec(p)); err == nil || !strings.Contains(err.Error(), "submodule") {
					t.Errorf("container: launch not refused: %v", err)
				}
			})
		}
	}
}

// A workspace with no .git/modules at all is the ordinary case and launches.
func TestModuleWalkNoModulesDirLaunches(t *testing.T) {
	l := newLayout(t)
	writeFile(t, filepath.Join(l.ws, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(l.ws, ".git", "config"), "[core]\n")
	if _, err := bwrapPlan(t, l, func(p *Profile, _ *Spec) { p.Network = false }); err != nil {
		t.Fatalf("bwrap: %v", err)
	}
}

// Round 4: every directory above a module git dir inside modules/ is pinned
// too — .git/modules itself, a "/"-named module's parent, a module's own
// modules/. Renamed away (inside one run on macOS, or between runs on a
// bind-mount backend), the modules below them sit at a path nothing holds.
func TestModuleParentDirsArePinned(t *testing.T) {
	l := newLayout(t)
	git, sub, _ := submoduleLayout(t, l.ws)
	writeFile(t, filepath.Join(git, "modules", "a", "b", "HEAD"), "ref: refs/heads/main\n")
	parents := []string{filepath.Join(git, "modules"), filepath.Join(git, "modules", "a"), filepath.Join(sub, "modules")}
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range parents {
		if !strings.Contains(plan.SBPL, `(deny file-write* (literal "`+d+`"))`) {
			t.Errorf("macOS: %s not pinned", d)
		}
	}
	cargs := strings.Join(containerPlan(t, l, nil).Argv, " ")
	bp, err := bwrapPlan(t, l, func(p *Profile, _ *Spec) { p.Network = false })
	if err != nil {
		t.Fatal(err)
	}
	bargs := strings.Join(bp.Argv, "\x00")
	for _, d := range parents {
		if !strings.Contains(cargs, "type=bind,source="+d+",target="+d+" ") {
			t.Errorf("container: %s not bound onto itself", d)
		}
		if !strings.Contains(bargs, "--bind\x00"+d+"\x00"+d+"\x00") {
			t.Errorf("bwrap: %s not bound onto itself", d)
		}
		// A pinned parent is not a git dir: no hooks or config.worktree is
		// created in it.
		for _, n := range []string{"hooks", "config.worktree"} {
			if _, err := os.Lstat(filepath.Join(d, n)); err == nil {
				t.Errorf("%s created in %s", n, d)
			}
		}
	}
}

// With no .git/modules at launch, macOS still refuses to create one (a
// modules dir built elsewhere and renamed into place would carry module
// configs no rule named).
func TestMacOSPinsAbsentModulesDir(t *testing.T) {
	l := newLayout(t)
	writeFile(t, filepath.Join(l.ws, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(l.ws, ".git", "config"), "[core]\n")
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := filepath.Join(l.ws, ".git", "modules"); !strings.Contains(plan.SBPL, `(deny file-write* (literal "`+d+`"))`) {
		t.Errorf("macOS: absent %s not pinned", d)
	}
}

// The walk also caps the directories it enters, so empty decoy dirs cannot
// turn into thousands of mounts.
func TestModuleWalkDirCapRefusesLaunch(t *testing.T) {
	l := newLayout(t)
	submoduleLayout(t, l.ws)
	for i := 0; i < maxModuleWalkDirs+10; i++ {
		if err := os.MkdirAll(filepath.Join(l.ws, ".git", "modules", fmt.Sprintf("e%05d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bwrapPlan(t, l, func(p *Profile, _ *Spec) { p.Network = false }); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Errorf("bwrap: launch not refused: %v", err)
	}
}

// R12 (gap12): container and bwrap never expose <rysh dir>/memory/entries.
// bwrap masks the whole rysh dir with an empty tmpfs; the container mounts
// nothing from it.
func TestMemoryEntriesNotExposedContainerBwrap(t *testing.T) {
	l := newLayout(t)
	entries := filepath.Join(l.rysh, "memory", "entries")
	writeFile(t, filepath.Join(entries, "e1.md"), "synthetic memory\n")
	// The only thing a sandbox exposes from the rysh dir is its own home.
	exposed := func(host string) bool {
		return within(host, l.rysh) && !within(host, l.sbxHome) || within(entries, host) && host != l.ws
	}
	cp := containerPlan(t, l, nil)
	masked := false
	for _, m := range cp.Mounts {
		masked = masked || m.Hidden && m.Host == l.rysh
		if !m.Hidden && exposed(m.Host) {
			t.Errorf("container exposes %s (target %s)", m.Host, m.Target)
		}
	}
	if !masked {
		t.Error("container does not mask the rysh dir")
	}
	bp, err := bwrapPlan(t, l, func(p *Profile, _ *Spec) { p.Network = false })
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Join(bp.Argv, "\x00")
	tmpfs := strings.Index(a, "--tmpfs\x00"+l.rysh+"\x00")
	if tmpfs < 0 {
		t.Fatal("bwrap does not mask the rysh dir")
	}
	for _, flag := range []string{"--bind", "--ro-bind"} {
		for i := tmpfs; ; {
			j := strings.Index(a[i:], flag+"\x00")
			if j < 0 {
				break
			}
			src := strings.SplitN(a[i+j+len(flag)+1:], "\x00", 2)[0]
			if exposed(src) {
				t.Errorf("bwrap re-binds %s after masking the rysh dir", src)
			}
			i += j + len(flag) + 1
		}
	}
}
