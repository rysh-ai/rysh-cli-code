// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// Container is the docker / podman backend (design 066 S1). docker is preferred
// when both are on PATH; neither is an error, never a fallback.
//
// What the container sees, and nothing else from the host:
//   - the workspace, read-write, at its own path (and its cwd);
//   - the sandbox home, read-write, at its own path, as HOME;
//   - a linked worktree's shared .git directory, read-write (commits need it);
//   - profile read_only paths and policy read-only paths, read-only;
//   - an empty tmpfs over the rysh directory (when it sits in the workspace) and
//     over every policy deny path that is a directory; /dev/null over a denied
//     file.
//
// The env crosses by NAME ONLY: `-e NAME` makes docker copy the value from its
// own environment, so no value is ever in argv. The image is run with --pull
// never; a missing image fails the launch.
type Container struct {
	LookPath LookPathFunc
	UID, GID int
}

func (c *Container) Name() string { return BackendContainer }

func (c *Container) tool() (string, error) {
	look := c.LookPath
	if look == nil {
		look = exec.LookPath
	}
	for _, t := range []string{"docker", "podman"} {
		if p, err := look(t); err == nil && p != "" {
			return p, nil
		}
	}
	return "", errors.New("sandbox: neither docker nor podman is on PATH — install one, or use a profile with another backend (the pane was not started)")
}

func (c *Container) Available() error {
	_, err := c.tool()
	return err
}

// containerHostDrop are dropped on top of innerDropNames: the container has its
// own filesystem layout, so the host's paths and shell state mean nothing there.
var containerHostDrop = map[string]bool{
	"PATH": true, "TMPDIR": true, "PWD": true, "OLDPWD": true, "SHLVL": true, "_": true,
	"USER": true, "LOGNAME": true, "SHELL": true, "XPC_SERVICE_NAME": true, "XPC_FLAGS": true,
	"__CF_USER_TEXT_ENCODING": true, "__CFBundleIdentifier": true,
}

// containerClientEnv are host names the docker/podman CLIENT reads to find its
// daemon; they stay in the tool's env and never cross into the container.
var containerClientEnv = map[string]bool{
	"HOME": true, "PATH": true, "TMPDIR": true, "XDG_RUNTIME_DIR": true, "XDG_CONFIG_HOME": true,
	"CONTAINER_HOST": true, "CONTAINERS_CONF": true,
}

func (c *Container) Prepare(s Spec) (*Plan, error) {
	if err := CheckSpec(s); err != nil {
		return nil, err
	}
	tool, err := c.tool()
	if err != nil {
		return nil, err
	}
	if s.Profile.Proxy {
		// Linux only: Select refuses it on a VM-backed docker (darwin, windows).
		if err := checkBridge(s); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(s.Profile.Image) == "" {
		return nil, fmt.Errorf("sandbox: profile %q uses the container backend but names no image (set image: in sandbox.yaml; rysh never pulls one)", s.Profile.Name)
	}
	if !ValidName(s.ID) {
		return nil, fmt.Errorf("sandbox: invalid sandbox id %q", s.ID)
	}

	var mounts []Mount
	add := func(m Mount) error {
		for _, p := range []string{m.Host, m.Target} {
			if strings.ContainsAny(p, ",\n\x00") {
				return fmt.Errorf("sandbox: path %q cannot be passed to --mount (contains a comma or newline)", p)
			}
		}
		mounts = append(mounts, m)
		return nil
	}
	// The workspace and the shared .git take the policy's verdict for their own
	// path (a read_only "**" makes the whole workspace read-only).
	if err := add(Mount{Host: s.Workspace, Target: s.Workspace, ReadOnly: verdictReadOnly(s.Workspace, s.Rules)}); err != nil {
		return nil, err
	}
	if err := add(Mount{Host: s.Home, Target: s.Home}); err != nil {
		return nil, err
	}
	rwAreas := []string{s.Workspace}
	if s.GitCommonDir != "" && !within(s.GitCommonDir, s.Workspace) {
		if err := add(Mount{Host: s.GitCommonDir, Target: s.GitCommonDir, ReadOnly: verdictReadOnly(s.GitCommonDir, s.Rules)}); err != nil {
			return nil, err
		}
		rwAreas = append(rwAreas, s.GitCommonDir)
	}
	// Policy read_only / read_write rules re-layer what is already read-write
	// here. Outside it a rule has nothing to restrict (the path is not mounted,
	// or mounted read-only), so it never adds a mount there.
	for _, l := range layered(s.Rules) {
		if !insideAny(l.Path, rwAreas) || isAnyOf(l.Path, rwAreas) || within(l.Path, s.RyshDir) || deniedByRules(l.Path, s.Rules) {
			continue
		}
		if _, err := os.Stat(l.Path); err != nil {
			// A missing path cannot be bind-mounted. A missing read_only path
			// the agent could create and write would break the rule, so that
			// refuses, unless its parent is read-only anyway; a missing
			// read_write carve-out just stays read-only (stricter).
			if l.ReadOnly && !verdictReadOnly(filepath.Dir(l.Path), s.Rules) {
				return nil, fmt.Errorf("sandbox: policy read-only path %s does not exist, so a container cannot hold it read-only — create it, or drop the rule", l.Path)
			}
			continue
		}
		if err := add(Mount{Host: l.Path, Target: l.Path, ReadOnly: l.ReadOnly}); err != nil {
			return nil, err
		}
	}
	// Git metadata (gap01 E2). Each existing git dir is bound onto itself — a
	// mount point cannot be renamed away (EBUSY) — and its hooks, config and
	// config.worktree are bound read-only on top. A missing hooks dir or
	// config.worktree is created empty first, or the agent could create it.
	if err := gitMounts(s, rwAreas, add); err != nil {
		return nil, err
	}
	for _, ro := range s.Profile.ReadOnly {
		if hiddenByPolicy(ro, s) {
			return nil, fmt.Errorf("sandbox: read-only path %s is one a sandbox never exposes", ro)
		}
		if _, err := os.Stat(ro); err != nil {
			return nil, fmt.Errorf("sandbox: read-only path %s: %w", ro, err)
		}
		if err := add(Mount{Host: ro, Target: ro, ReadOnly: true}); err != nil {
			return nil, err
		}
	}
	// A deny path is hidden where the container could otherwise see it. A
	// missing one has nothing to hide, and a mount target inside a bind would
	// make docker create it on the host.
	visible := append(append([]string(nil), rwAreas...), s.Profile.ReadOnly...)
	var hide []string
	for _, d := range s.Rules.Deny {
		if _, err := os.Lstat(d); err == nil && insideAny(d, visible) {
			hide = append(hide, d)
		}
	}
	if s.RyshDir != "" && within(s.RyshDir, s.Workspace) {
		hide = append(hide, s.RyshDir)
	}
	for _, sp := range SensitivePaths(s.HostHome) {
		if within(sp, s.Workspace) {
			hide = append(hide, sp) // a workspace can never contain $HOME, but be exact
		}
	}
	for _, h := range hide {
		if err := add(Mount{Host: h, Target: h, Hidden: true}); err != nil {
			return nil, err
		}
	}
	// The sandbox home sits under the rysh directory, which is hidden. Mounts
	// are applied shallowest first, so the deeper home bind lands ON TOP of the
	// tmpfs; ordering them here keeps that true whatever the tool does.
	sort.SliceStable(mounts, func(i, j int) bool { return depth(mounts[i].Target) < depth(mounts[j].Target) })

	name := "rysh-sbx-" + s.ID
	marker := BackendContainer + "/" + s.Profile.Name
	drop, set := s.innerParts(containerHostDrop)
	inner := innerEnv(s.Env, drop, set, s.Home, marker)

	argv := []string{tool, "run", "--rm", "-i"}
	if c.isTTY() {
		argv = append(argv, "-t")
	}
	argv = append(argv,
		"--name", name,
		"--pull", "never",
		"--init",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", strconv.Itoa(c.UID)+":"+strconv.Itoa(c.GID),
		"--workdir", s.Workspace,
	)
	if !s.Profile.Network {
		argv = append(argv, "--network", "none")
	}
	if s.Profile.Proxy {
		argv = append(argv,
			"--mount", "type=bind,source="+s.Bridge.Dir+",target="+BridgeMount,
			"--mount", "type=bind,source="+s.Bridge.Binary+",target="+BridgeMount+"/rysh,readonly")
	}
	for _, m := range mounts {
		a := mountArg(m)
		// A hidden tmpfs with another mount beneath it (the sandbox home under
		// the rysh dir) must be traversable by the non-root user: docker's
		// default tmpfs is mode 700, owned by root. 0711 lets the user pass
		// through without listing; the tmpfs is empty anyway.
		if m.Hidden && strings.HasPrefix(a, "type=tmpfs,") && hasMountBeneath(m.Target, mounts) {
			a += ",tmpfs-mode=0711"
		}
		argv = append(argv, "--mount", a)
	}
	// HOME and the marker are set EXPLICITLY (paths and a label, not secrets):
	// the tool process itself keeps the host HOME, which is how the docker
	// client finds its context (colima, Docker Desktop). Every other name
	// crosses by NAME only, its value read from the tool's own env.
	for _, name := range envNames(inner) {
		switch name {
		case "HOME":
			argv = append(argv, "-e", "HOME="+s.Home)
		case MarkerEnv:
			argv = append(argv, "-e", MarkerEnv+"="+marker)
		default:
			if kv, ok := isSetEnv(s.SetEnv, name); ok {
				argv = append(argv, "-e", kv) // a sandbox-home path (CheckSpec)
				continue
			}
			argv = append(argv, "-e", name)
		}
	}
	argv = append(argv, s.Profile.Image)
	if s.Profile.Proxy {
		argv = append(argv, forwardArgv(s.Bridge.Port, s.Argv[0], s.Argv[1:])...)
	} else {
		argv = append(argv, s.Argv...)
	}

	// The TOOL's env: the inner env (so `-e NAME` has its values) minus the two
	// set explicitly above, plus what the client needs to reach its daemon.
	toolEnv := make([]string, 0, len(inner)+8)
	for _, kv := range inner {
		name, _, _ := strings.Cut(kv, "=")
		if _, set := isSetEnv(s.SetEnv, name); name != "HOME" && name != MarkerEnv && !set {
			toolEnv = append(toolEnv, kv)
		}
	}
	for _, kv := range s.Env {
		name, _, _ := strings.Cut(kv, "=")
		if containerClientEnv[name] || strings.HasPrefix(name, "DOCKER_") || strings.HasPrefix(name, "PODMAN_") {
			toolEnv = append(toolEnv, kv)
		}
	}

	return &Plan{
		Backend:       BackendContainer,
		Profile:       s.Profile.Name,
		Argv:          argv,
		Env:           toolEnv,
		InnerEnvNames: envNames(inner),
		Mounts:        mounts,
		ContainerName: name,
		Tool:          tool,
	}, nil
}

// gitMounts adds the git metadata mounts (gap01 E2) for a backend that
// exposes paths by bind mount (container, bwrap). Each git dir is bound onto
// itself (a mount point cannot be renamed away: EBUSY) with its metadata bound
// read-only on top; each pointer FILE (a linked worktree's or a submodule's
// .git) is bound read-only, so it can be neither moved nor rewritten. The
// directories between a git dir and its submodule git dirs are bound onto
// themselves too, with nothing on top (GitModuleParents).
func gitMounts(s Spec, rwAreas []string, add func(Mount) error) error {
	pins, err := GitPins(s.Workspace, s.GitCommonDir)
	if err != nil {
		return err
	}
	parents, err := GitModuleParents(s.Workspace, s.GitCommonDir)
	if err != nil {
		return err
	}
	for _, p := range parents {
		if !insideAny(p, rwAreas) || within(p, s.RyshDir) || deniedByRules(p, s.Rules) || isAnyOf(p, rwAreas) {
			continue
		}
		if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
			continue // not there (or a link we never follow)
		}
		if err := add(Mount{Host: p, Target: p, ReadOnly: verdictReadOnly(p, s.Rules)}); err != nil {
			return err
		}
	}
	for _, p := range pins {
		if !insideAny(p, rwAreas) || within(p, s.RyshDir) || deniedByRules(p, s.Rules) {
			continue
		}
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			continue // no repo here (or a link we never follow)
		}
		if !fi.IsDir() {
			if err := add(Mount{Host: p, Target: p, ReadOnly: true}); err != nil {
				return err
			}
			continue
		}
		if !isAnyOf(p, rwAreas) {
			if err := add(Mount{Host: p, Target: p, ReadOnly: verdictReadOnly(p, s.Rules)}); err != nil {
				return err
			}
		}
		for _, n := range gitMetaNames {
			mp := filepath.Join(p, n)
			if _, err := os.Lstat(mp); os.IsNotExist(err) && gitMetaCreatable[n] {
				var cerr error
				if n == "hooks" {
					cerr = os.MkdirAll(mp, 0o755)
				} else {
					// Empty: git reads config.worktree only when the read-only
					// config turns extensions.worktreeConfig on.
					cerr = os.WriteFile(mp, nil, 0o644)
				}
				if cerr != nil {
					return fmt.Errorf("sandbox: prepare %s: %w", mp, cerr)
				}
			}
			if pfi, err := os.Lstat(mp); err != nil || pfi.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if err := add(Mount{Host: mp, Target: mp, ReadOnly: true}); err != nil {
				return err
			}
		}
	}
	return nil
}

// stdinIsTTY decides -t. A character-device check is NOT enough: /dev/null
// is one too, and `docker run -t` with a non-terminal stdin refuses to start
// (found by the gated real-docker run). Overridable so tests are deterministic.
var stdinIsTTY = realStdinIsTTY

func realStdinIsTTY() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

func (c *Container) isTTY() bool { return stdinIsTTY() }

func hasMountBeneath(target string, mounts []Mount) bool {
	for _, o := range mounts {
		if o.Target != target && within(o.Target, target) {
			return true
		}
	}
	return false
}

func insideAny(p string, bases []string) bool {
	for _, b := range bases {
		if within(p, b) {
			return true
		}
	}
	return false
}

func isAnyOf(p string, list []string) bool {
	for _, b := range list {
		if p == b {
			return true
		}
	}
	return false
}

func depth(p string) int { return strings.Count(filepath.Clean(p), string(filepath.Separator)) }

func mountArg(m Mount) string {
	if m.Hidden {
		if fi, err := os.Stat(m.Host); err == nil && !fi.IsDir() {
			return "type=bind,source=/dev/null,target=" + m.Target + ",readonly"
		}
		return "type=tmpfs,target=" + m.Target
	}
	a := "type=bind,source=" + m.Host + ",target=" + m.Target
	if m.ReadOnly {
		a += ",readonly"
	}
	return a
}

// hiddenByPolicy reports whether p is (or is inside) a path the sandbox always
// hides, or that a deny rule hides.
func hiddenByPolicy(p string, s Spec) bool {
	for _, sp := range SensitivePaths(s.HostHome) {
		if within(p, sp) || within(sp, p) {
			return true
		}
	}
	if within(s.HostHome, p) {
		return true
	}
	for _, d := range s.Rules.Deny {
		if within(p, d) {
			return true
		}
	}
	return false
}

// Stop removes the container (it is --rm, so this only matters while the agent
// is still running). The tool runs with an empty env apart from PATH.
func (c *Container) Stop(p *Plan) error {
	if p == nil || p.ContainerName == "" || p.Tool == "" {
		return nil
	}
	cmd := exec.Command(p.Tool, "rm", "-f", p.ContainerName)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: %s rm -f %s: %v: %s", p.Tool, p.ContainerName, err, strings.TrimSpace(string(out)))
	}
	return nil
}
