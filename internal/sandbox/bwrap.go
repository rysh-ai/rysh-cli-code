// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Bwrap is the Linux bubblewrap backend (gap01 E5): an OS sandbox in new
// namespaces, with no daemon and no image, the way Linux desktop agents are
// usually confined. bwrap starts from an EMPTY root; the agent sees only what
// is bound into it:
//   - /usr, /bin, /sbin, /lib*, /etc read-only (the OS the agent runs on);
//   - a fresh /proc, /dev and /tmp;
//   - the workspace read-write at its own path (its cwd), the sandbox home
//     read-write as HOME, a linked worktree's shared .git;
//   - the agent's own install directory read-only;
//   - profile and policy read-only paths read-only, policy deny paths and the
//     rysh dir masked; git hooks and config read-only (as the container).
//
// Nothing else of $HOME exists inside: there is no path to ~/.ssh at all.
// `network: off` unshares the network namespace (only loopback, empty).
// `network: proxy` unshares it too, and reaches the governance proxy through
// the bridge (bridge.go). `network: on` is REFUSED: it would share the host's
// namespace, and with it the host's loopback services.
//
// The env reaches the agent through bwrap's own exec environment; no value is
// ever in argv.
type Bwrap struct {
	LookPath LookPathFunc
	// ResolveAgent finds the agent on the FILTERED env's PATH.
	ResolveAgent func(name string, env []string) (string, error)
}

func (b *Bwrap) Name() string { return BackendBwrap }

func (b *Bwrap) tool() (string, error) {
	look := b.LookPath
	if look == nil {
		look = exec.LookPath
	}
	p, err := look("bwrap")
	if err != nil || p == "" {
		return "", errors.New("sandbox: bwrap (bubblewrap) is not on PATH — install it, or use a container profile (the pane was not started)")
	}
	return p, nil
}

func (b *Bwrap) Available() error {
	_, err := b.tool()
	return err
}

func (b *Bwrap) resolve(name string, env []string) (string, error) {
	if b.ResolveAgent != nil {
		return b.ResolveAgent(name, env)
	}
	return resolveInPath(name, env)
}

// bwrapSystemDirs are bound read-only when they exist on the host.
var bwrapSystemDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/etc"}

// bwrapHostDrop are host shell state that means nothing in the new root.
var bwrapHostDrop = map[string]bool{"TMPDIR": true, "PWD": true, "OLDPWD": true, "XDG_RUNTIME_DIR": true}

// errBwrapNetworkOn refuses bwrap with the network on: bwrap would share the
// host's network namespace, so host loopback services (the unauthenticated
// session bus among them) would be reachable from inside.
func errBwrapNetworkOn(profile string) error {
	return fmt.Errorf("sandbox: profile %q: bwrap with network on would share the host's network (and its loopback services); use network: off, or a container profile", profile)
}

func (b *Bwrap) Prepare(s Spec) (*Plan, error) {
	if err := CheckSpec(s); err != nil {
		return nil, err
	}
	if s.Profile.Proxy {
		if err := checkBridge(s); err != nil {
			return nil, err
		}
	}
	if s.Profile.Network {
		return nil, errBwrapNetworkOn(s.Profile.Name)
	}
	tool, err := b.tool()
	if err != nil {
		return nil, err
	}
	agent, readDirs, readLits, err := agentReadPaths(s, b.resolve)
	if err != nil {
		return nil, err
	}

	var mounts []Mount
	add := func(m Mount) error {
		if strings.ContainsAny(m.Host+m.Target, "\x00") {
			return fmt.Errorf("sandbox: path %q cannot be bound", m.Host)
		}
		mounts = append(mounts, m)
		return nil
	}
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
	for _, l := range layered(s.Rules) {
		if !insideAny(l.Path, rwAreas) || isAnyOf(l.Path, rwAreas) || within(l.Path, s.RyshDir) || deniedByRules(l.Path, s.Rules) {
			continue
		}
		if _, err := os.Stat(l.Path); err != nil {
			if l.ReadOnly && !verdictReadOnly(parentDir(l.Path), s.Rules) {
				return nil, fmt.Errorf("sandbox: policy read-only path %s does not exist, so bwrap cannot hold it read-only — create it, or drop the rule", l.Path)
			}
			continue
		}
		if err := add(Mount{Host: l.Path, Target: l.Path, ReadOnly: l.ReadOnly}); err != nil {
			return nil, err
		}
	}
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
	for _, d := range append(append([]string(nil), readDirs...), readLits...) {
		if insideAny(d, rwAreas) || within(d, s.Home) {
			continue // already visible
		}
		if err := add(Mount{Host: d, Target: d, ReadOnly: true}); err != nil {
			return nil, err
		}
	}
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
	for _, h := range hide {
		if err := add(Mount{Host: h, Target: h, Hidden: true}); err != nil {
			return nil, err
		}
	}
	// Later binds land on top of earlier ones: shallowest first, so the
	// sandbox home bind sits on the rysh-dir tmpfs above it.
	sort.SliceStable(mounts, func(i, j int) bool { return depth(mounts[i].Target) < depth(mounts[j].Target) })

	argv := []string{tool, "--die-with-parent", "--new-session", "--unshare-all"}
	for _, d := range bwrapSystemDirs {
		argv = append(argv, "--ro-bind-try", d, d)
	}
	argv = append(argv, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	for _, m := range mounts {
		switch {
		case m.Hidden:
			if fi, err := os.Stat(m.Host); err == nil && !fi.IsDir() {
				argv = append(argv, "--ro-bind", "/dev/null", m.Target)
			} else {
				argv = append(argv, "--tmpfs", m.Target)
			}
		case m.ReadOnly:
			argv = append(argv, "--ro-bind", m.Host, m.Target)
		default:
			argv = append(argv, "--bind", m.Host, m.Target)
		}
	}
	argv = append(argv, "--chdir", s.Workspace)
	if s.Profile.Proxy {
		// The bridge dir and the forwarder binary; the forwarder runs the agent.
		argv = append(argv, "--bind", s.Bridge.Dir, BridgeMount, "--ro-bind", s.Bridge.Binary, BridgeMount+"/rysh")
		argv = append(argv, forwardArgv(s.Bridge.Port, agent, s.Argv[1:])...)
	} else {
		argv = append(argv, agent)
		argv = append(argv, s.Argv[1:]...)
	}

	marker := BackendBwrap + "/" + s.Profile.Name
	drop, set := s.innerParts(bwrapHostDrop)
	inner := innerEnv(s.Env, drop, set, s.Home, marker)
	return &Plan{
		Backend:       BackendBwrap,
		Profile:       s.Profile.Name,
		Argv:          argv,
		Env:           inner,
		InnerEnvNames: envNames(inner),
		Mounts:        mounts,
		Tool:          tool,
	}, nil
}

func parentDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

// Stop has nothing to tear down: bwrap IS the agent process (exec'd in place),
// started with --die-with-parent.
func (b *Bwrap) Stop(*Plan) error { return nil }
