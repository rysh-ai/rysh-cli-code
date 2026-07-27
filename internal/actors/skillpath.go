package actors

import (
	"log/slog"
	"os"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"
)

// ryshBaseDirs returns the ordered list of .rysh directories rysh should search
// for skill files, secrets, and variables. The project-local ./.rysh (when
// present) is preferred, followed by $HOME/.rysh. Callers walk the list and use
// the first hit.
//
// The $HOME fallback is deliberate for a process sitting in a directory that
// simply has no .rysh — that is how global skills and secrets are found. It is
// NOT safe when the working directory has been DELETED out from under a running
// process (a git worktree removed while its daemon still ran, a build dir
// cleaned up). Both cases look identical to `os.Stat(".rysh")`: it fails, the
// local base drops out of the list, and $HOME/.rysh silently becomes the only
// base — promoting a project-scoped process to the user's global state, for
// reads AND writes, with nothing logged. A stray daemon did exactly that to a
// real ~/.rysh.
//
// So the two cases are separated by asking whether the working directory itself
// still exists. When it does not, there is no correct answer — the process
// cannot resolve project-local paths and must not adopt the user's global ones
// — so only the local base is returned. Every subsequent open then fails with
// ENOENT against the dead directory, loudly and locally, instead of quietly
// succeeding somewhere it was never pointed at.
func ryshBaseDirs() []string {
	if !workingDirAlive() {
		slog.Error(progname.Rewrite("rysh: working directory has been deleted — refusing to resolve .rysh paths against $HOME; ") +
			"restart the process from a directory that exists")
		return []string{".rysh"}
	}

	var dirs []string
	if info, err := os.Stat(".rysh"); err == nil && info.IsDir() {
		dirs = append(dirs, ".rysh")
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".rysh"))
	}
	if len(dirs) == 0 {
		dirs = append(dirs, ".rysh")
	}
	return dirs
}

// workingDirAlive reports whether the process's working directory still exists
// on disk.
//
// The obvious checks do not work. On macOS/APFS a deleted directory keeps its
// vnode for as long as a process holds it as cwd, so os.Stat("."), os.Open("."),
// os.ReadDir(".") and even os.Getwd() all still succeed, and the link count
// stays at 2 — measured, not assumed. Resolving the cwd to an absolute path and
// stat-ing THAT is what detects the unlink: Getwd still reports where the
// process used to be, and nothing is there any more. On Linux os.Getwd fails
// outright for a deleted cwd, which the first branch covers.
func workingDirAlive() bool {
	wd, err := os.Getwd()
	if err != nil {
		return false
	}
	_, err = os.Stat(wd)
	return err == nil
}

// resolveRyshPath resolves a user-supplied name for skill-file commands like
// ##agent spawn or ##humanoid spawn. Explicit paths pass through:
//   - absolute paths (/abs/path)
//   - anything starting with ~/ (home expanded)
//   - anything containing a path separator (./foo, ../foo, sub/foo)
//
// Bare names like "slack-bot" resolve to <base>/<subdir>/<name>. The function
// walks the prioritized base-dir list and returns the first candidate that
// exists on disk; if none match, the highest-priority candidate is returned
// so the resulting "file not found" error references the expected location.
//
// An empty name returns <base>/<subdir>, the parent directory used by
// spawn-all to iterate per-entity subdirectories.
func resolveRyshPath(subdir, p string) string {
	bases := ryshBaseDirs()

	if p == "" {
		for _, b := range bases {
			candidate := filepath.Join(b, subdir)
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate
			}
		}
		return filepath.Join(bases[0], subdir)
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
		return p
	}
	if filepath.IsAbs(p) || strings.ContainsRune(p, filepath.Separator) {
		return p
	}
	for _, b := range bases {
		candidate := filepath.Join(b, subdir, p)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join(bases[0], subdir, p)
}

// skillFilePath resolves a user-supplied name/path to the skill FILE that
// defines an entity: resolveRyshPath, then the entity directory's SKILL.md.
// A name that resolves to nothing on disk still gets the SKILL.md suffix, so
// "not found" errors name the file callers were actually looking for rather
// than its parent directory.
func skillFilePath(subdir, p string) string {
	path := resolveRyshPath(subdir, p)
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return filepath.Join(path, "SKILL.md")
		}
		return path
	}
	if strings.HasSuffix(path, ".md") {
		return path
	}
	return filepath.Join(path, "SKILL.md")
}

// deriveSkillName picks a sensible name from a skill-file path. If the file is
// named SKILL.md, the parent directory's name (the entity dir) is used so a
// frontmatter-less SKILL.md gets the expected name. Otherwise the filename
// without extension is used.
func deriveSkillName(path string) string {
	base := filepath.Base(path)
	if base == "SKILL.md" {
		return filepath.Base(filepath.Dir(path))
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}
