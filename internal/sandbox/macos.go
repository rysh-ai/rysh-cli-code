// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MacOS is the sandbox-exec backend (design 066 S2).
//
// DEPRECATION, stated plainly: Apple's sandbox-exec(1) man page has marked the
// tool deprecated for years. It still ships in every macOS release and is what
// Chrome's helpers, the Codex CLI and Claude Code's own sandbox mode use to
// confine a process; the supported alternatives (App Sandbox entitlements, a
// virtual machine, an Endpoint Security client) need code signing or a
// hypervisor and cannot wrap an arbitrary CLI the user already has installed.
// So rysh uses it, and if Apple removes it, Available fails and every sandboxed
// pane fails closed — it never starts unsandboxed.
//
// The generated profile (SBPL; the LAST matching rule wins):
//   - everything is allowed by default (the agent's own runtime needs a lot of
//     the OS), then
//   - writes are denied everywhere except the workspace, the sandbox home, a
//     linked worktree's shared .git, the per-user temp dir, /private/tmp, /dev;
//   - reading file CONTENTS under the real $HOME is denied except the workspace,
//     the sandbox home, the shared .git, the agent's own install directory and
//     profile read_only paths (metadata — names, sizes — stays readable: path
//     resolution needs it);
//   - read_only paths and policy read-only paths cannot be written;
//   - the rysh dir, the always-sensitive home paths (~/.ssh, ~/.aws,
//     ~/.rysh_env, ~/Library/Keychains, the host ~/.claude …) and policy deny
//     paths can be neither read nor written, whatever an earlier allow said;
//   - the keychain's mach services are not reachable;
//   - `network: off` denies all network.
type MacOS struct {
	// ToolPath is sandbox-exec's absolute path; never resolved from PATH, so a
	// shadowing binary on PATH cannot replace it.
	ToolPath string
	// ToolOK checks the tool exists (os.Stat in production).
	ToolOK func(string) error
	// ResolveAgent finds the agent binary on the FILTERED env's PATH.
	ResolveAgent func(name string, env []string) (string, error)
	LookPath     LookPathFunc
}

// readOps is how every read rule after the first allow is spelled. SBPL does
// NOT let a later wildcard override an earlier specific operation: a
// `(deny file-read* …)` after `(allow file-read-data …)` leaves the data
// readable (found by the gated E2E test, and reproduced by hand). So each
// override names file-read-data explicitly next to the wildcard.
const readOps = "file-read* file-read-data"

// allowedUnixSockets are the only unix sockets a network-on sandbox may
// connect to: the system DNS resolver and syslog.
var allowedUnixSockets = []string{"/private/var/run/mDNSResponder", "/private/var/run/syslog"}

// DefaultSandboxExec is where macOS ships the tool.
const DefaultSandboxExec = "/usr/bin/sandbox-exec"

func (m *MacOS) Name() string { return BackendMacOS }

func (m *MacOS) toolPath() string {
	if m.ToolPath != "" {
		return m.ToolPath
	}
	return DefaultSandboxExec
}

func (m *MacOS) Available() error {
	ok := m.ToolOK
	if ok == nil {
		ok = func(p string) error { _, err := os.Stat(p); return err }
	}
	if err := ok(m.toolPath()); err != nil {
		return fmt.Errorf("sandbox: %s is not available (%v) — Apple has deprecated sandbox-exec; use a container profile instead", m.toolPath(), err)
	}
	return nil
}

func (m *MacOS) resolve(name string, env []string) (string, error) {
	if m.ResolveAgent != nil {
		return m.ResolveAgent(name, env)
	}
	return resolveInPath(name, env)
}

// resolveInPath finds name on env's PATH (the env the agent will run with).
func resolveInPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		if _, err := os.Stat(name); err != nil {
			return "", err
		}
		return filepath.Abs(name)
	}
	for _, kv := range env {
		v, ok := strings.CutPrefix(kv, "PATH=")
		if !ok {
			continue
		}
		for _, dir := range filepath.SplitList(v) {
			if dir == "" || !filepath.IsAbs(dir) {
				continue // never the cwd: the workspace is the agent's to write
			}
			p := filepath.Join(dir, name)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%s: not found in PATH", name)
}

// scriptInterpreter is the program script's shebang names, with an env(1)
// target resolved by resolve, or "" for a binary.
func scriptInterpreter(script string, env []string, resolve func(string, []string) (string, error)) string {
	f, err := os.Open(script)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	rest, ok := strings.CutPrefix(line, "#!")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	prog := fields[0]
	if filepath.Base(prog) == "env" {
		for _, a := range fields[1:] {
			if strings.HasPrefix(a, "-") {
				continue
			}
			p, err := resolve(a, env)
			if err != nil {
				return ""
			}
			return p
		}
		return ""
	}
	return prog
}

// agentReadPaths resolves the agent and what it must READ to start at all:
// the directory its real file is in, and for a script the install prefix of
// its interpreter (bin/.. — node's lib/ lives there); readLits are the
// symlinks on the way. Shared by the backends that expose files by path
// (macOS, bwrap). It refuses an agent whose install would expose $HOME or a
// sensitive path.
func agentReadPaths(s Spec, resolve func(string, []string) (string, error)) (agent string, readDirs, readLits []string, err error) {
	agent, err = resolve(s.Argv[0], s.Env)
	if err != nil {
		return "", nil, nil, fmt.Errorf("sandbox: agent %q: %w", s.Argv[0], err)
	}
	real, err := filepath.EvalSymlinks(agent)
	if err != nil {
		return "", nil, nil, fmt.Errorf("sandbox: agent %s: %w", agent, err)
	}
	readDirs = append(readDirs, filepath.Dir(real))
	if agent != real {
		readLits = append(readLits, agent)
	}
	if interp := scriptInterpreter(real, s.Env, resolve); interp != "" {
		if ri, err := filepath.EvalSymlinks(interp); err == nil {
			readDirs = append(readDirs, filepath.Dir(filepath.Dir(ri)))
			if ri != interp {
				readLits = append(readLits, interp)
			}
		}
	}
	for _, d := range readDirs {
		if within(s.HostHome, d) {
			return "", nil, nil, fmt.Errorf("sandbox: the agent lives directly in %s; reading it would expose your whole home directory", d)
		}
		for _, sp := range SensitivePaths(s.HostHome) {
			if within(d, sp) && !isAgentInstallUnderClaude(d, s.HostHome) {
				return "", nil, nil, fmt.Errorf("sandbox: the agent lives in %s, which a sandbox never exposes", d)
			}
		}
	}
	return agent, readDirs, readLits, nil
}

func (m *MacOS) Prepare(s Spec) (*Plan, error) {
	if err := CheckSpec(s); err != nil {
		return nil, err
	}
	if err := m.Available(); err != nil {
		return nil, err
	}
	agent, readDirs, readLits, err := agentReadPaths(s, m.resolve)
	if err != nil {
		return nil, err
	}
	// The one loopback port the profile may open: the session's governance
	// proxy, and only when a provider URL names it. CheckSpec has already
	// refused any other loopback port and any bus port.
	var proxyPorts []int
	for _, p := range LoopbackProxyPorts(s.Env) {
		if s.ProxyPort != 0 && p == s.ProxyPort {
			proxyPorts = append(proxyPorts, p)
		}
	}
	for _, ro := range s.Profile.ReadOnly {
		if hiddenByPolicy(ro, s) {
			return nil, fmt.Errorf("sandbox: read-only path %s is one a sandbox never exposes", ro)
		}
	}

	var writeDirs []string
	writeDirs = append(writeDirs, s.Workspace, s.Home)
	if s.GitCommonDir != "" {
		writeDirs = append(writeDirs, s.GitCommonDir)
	}
	writeDirs = append(writeDirs, tempDirs(s.Env)...)

	readable := append([]string{s.Workspace, s.Home}, readDirs...)
	if s.GitCommonDir != "" {
		readable = append(readable, s.GitCommonDir)
	}
	readable = append(readable, s.Profile.ReadOnly...)

	hidden := []string{}
	hidden = append(hidden, SensitivePaths(s.HostHome)...)
	hidden = append(hidden, s.Rules.Deny...)

	var b strings.Builder
	q := func(p string) (string, error) {
		if strings.ContainsAny(p, "\"\\\n\x00") {
			return "", fmt.Errorf("sandbox: path %q cannot be written into a sandbox profile", p)
		}
		return `"` + p + `"`, nil
	}
	rule := func(head, kind string, paths []string) error {
		if len(paths) == 0 {
			return nil
		}
		b.WriteString("(" + head)
		for _, p := range paths {
			qp, err := q(p)
			if err != nil {
				return err
			}
			b.WriteString(" (" + kind + " " + qp + ")")
		}
		b.WriteString(")\n")
		return nil
	}
	b.WriteString("(version 1)\n(allow default)\n")
	b.WriteString("(deny file-write*)\n")
	if err := rule("allow file-write*", "subpath", append(writeDirs, "/private/tmp", "/dev")); err != nil {
		return nil, err
	}
	if err := rule("deny file-read-data", "subpath", []string{s.HostHome}); err != nil {
		return nil, err
	}
	if err := rule("allow file-read-data", "subpath", readable); err != nil {
		return nil, err
	}
	if err := rule("allow "+readOps, "literal", readLits); err != nil {
		return nil, err
	}
	if err := rule("deny file-write*", "subpath", s.Profile.ReadOnly); err != nil {
		return nil, err
	}
	// Policy read_only / read_write, shallowest first: the last matching rule
	// wins, so a deeper rule overrides the one it sits in. A read_write
	// carve-out only re-opens writes inside the workspace or the shared .git,
	// never anywhere this profile did not already allow.
	rwAreas := []string{s.Workspace}
	if s.GitCommonDir != "" {
		rwAreas = append(rwAreas, s.GitCommonDir)
	}
	for _, l := range layered(s.Rules) {
		switch {
		case l.ReadOnly:
			if err := rule("deny file-write*", "subpath", []string{l.Path}); err != nil {
				return nil, err
			}
		case insideAny(l.Path, rwAreas):
			if err := rule("allow file-write*", "subpath", []string{l.Path}); err != nil {
				return nil, err
			}
		}
	}
	if s.RyshDir != "" {
		if err := rule("deny "+readOps+" file-write*", "subpath", []string{s.RyshDir}); err != nil {
			return nil, err
		}
		if err := rule("allow "+readOps+" file-write*", "subpath", []string{s.Home}); err != nil {
			return nil, err
		}
	}
	for _, h := range hidden {
		if err := rule("deny "+readOps+" file-write*", "subpath", []string{h}); err != nil {
			return nil, err
		}
	}
	// An agent installed under ~/.claude (claude's old `local` install) is
	// re-allowed for reading only, after the deny of ~/.claude above.
	for _, d := range readDirs {
		if isAgentInstallUnderClaude(d, s.HostHome) {
			if err := rule("allow "+readOps, "subpath", []string{d}); err != nil {
				return nil, err
			}
		}
	}
	// Git metadata that makes git run code is read-only, and the git dirs
	// themselves cannot be renamed or replaced (gap01 E2). After every write
	// allow above, policy carve-outs included, so nothing re-opens it.
	metaPaths, err := GitMetaPaths(s.Workspace, s.GitCommonDir)
	if err != nil {
		return nil, err
	}
	pins, err := GitPins(s.Workspace, s.GitCommonDir)
	if err != nil {
		return nil, err
	}
	for _, p := range metaPaths {
		kind := "literal"
		if filepath.Base(p) == "hooks" {
			kind = "subpath"
		}
		if err := rule("deny file-write*", kind, []string{p}); err != nil {
			return nil, err
		}
	}
	for _, pin := range pins {
		if err := rule("deny file-write*", "literal", []string{pin}); err != nil {
			return nil, err
		}
	}
	parents, err := GitModuleParents(s.Workspace, s.GitCommonDir)
	if err != nil {
		return nil, err
	}
	for _, p := range parents {
		if err := rule("deny file-write*", "literal", []string{p}); err != nil {
			return nil, err
		}
	}
	// Submodule git dirs created DURING the run are held by pattern: any
	// modules/…/<meta name> below a git dir.
	for _, d := range gitBaseDirs(s.Workspace, s.GitCommonDir) {
		if strings.ContainsAny(d, "\"\n\x00") {
			return nil, fmt.Errorf("sandbox: path %q cannot be written into a sandbox profile", d)
		}
		fmt.Fprintf(&b, "(deny file-write* (regex #\"^%s/modules/(.+/)?(hooks(/.*)?|config|config\\.worktree|commondir|gitdir)$\"))\n", regexQuoteSBPL(d))
	}
	// A NEW .git anywhere under the workspace (a dir, a pointer file, or a
	// rename into place) is refused, and the metadata of every nested
	// repository is held: a gitlink plus a fresh or existing sub/.git would
	// otherwise hand the user's next host-side `git status` a config the agent
	// wrote (LEAD-CHANGES round 3, 2b). The sandbox matches these paths
	// case-insensitively on APFS, so .GIT and CONFIG are held too (tested).
	if strings.ContainsAny(s.Workspace, "\"\n\x00") {
		return nil, fmt.Errorf("sandbox: path %q cannot be written into a sandbox profile", s.Workspace)
	}
	ws := regexQuoteSBPL(s.Workspace)
	fmt.Fprintf(&b, "(deny file-write* (regex #\"^%s/(.+/)?\\.git$\"))\n", ws)
	fmt.Fprintf(&b, "(deny file-write* (regex #\"^%s/(.+/)?\\.git/(hooks(/.*)?|config|config\\.worktree|commondir|gitdir)$\"))\n", ws)
	// The keychain, and the two services through which a sandboxed process
	// asks an UNSANDBOXED app to act for it: LaunchServices (`open -a …`) and
	// AppleEvents (`osascript -e 'tell app "Terminal" to do script …'`).
	b.WriteString(`(deny mach-lookup (global-name "com.apple.SecurityServer") (global-name "com.apple.securityd") (global-name "com.apple.securityd.xpc") (global-name "com.apple.security.agent")` +
		` (global-name "com.apple.coreservices.launchservicesd") (global-name "com.apple.coreservices.appleevents"))` + "\n")
	if !s.Profile.Network {
		b.WriteString("(deny network*)\n")
		if s.Profile.Proxy {
			// network: proxy — the governance proxy's loopback port only.
			for _, port := range proxyPorts {
				fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", port)
			}
		}
	} else {
		// Network on is for the model and the internet, not the host: the
		// session NATS (unauthenticated, on loopback) and other processes' unix
		// sockets (claude inboxes, tmux, docker, peers) would let the agent
		// drive unsandboxed panes (gap01 E1). Loopback is closed except the
		// agent's own provider port; unix sockets except the system resolver
		// and syslog.
		// Unix sockets are matched by PATH filters. A bare
		// `(remote unix-socket)` matches EVERY outbound connection, TCP
		// included (found by hand: it closed the provider port and the LAN),
		// so it is never used here.
		b.WriteString(`(deny network-outbound (subpath "/"))` + "\n")
		for _, sock := range allowedUnixSockets {
			fmt.Fprintf(&b, "(allow network-outbound (literal %q))\n", sock)
		}
		// The path deny has been seen to catch TCP connects too (EPERM on the
		// proxy port about one run in three when it came last), so TCP is
		// re-allowed explicitly — this never re-opens a unix socket (checked
		// by hand) — and loopback comes LAST: the proxy-port allow is the
		// final rule a TCP connect matches. The host's own non-loopback
		// addresses count as localhost to the sandbox and stay closed.
		b.WriteString(`(allow network-outbound (remote ip "*:*"))` + "\n")
		b.WriteString(`(deny network-outbound (remote ip "localhost:*"))` + "\n")
		for _, port := range proxyPorts {
			fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", port)
		}
	}
	sbpl := b.String()

	marker := BackendMacOS + "/" + s.Profile.Name
	drop, set := s.innerParts(nil)
	inner := innerEnv(s.Env, drop, set, s.Home, marker)
	argv := append([]string{m.toolPath(), "-p", sbpl, agent}, s.Argv[1:]...)

	mounts := []Mount{{Host: s.Workspace, Target: s.Workspace}, {Host: s.Home, Target: s.Home}}
	for _, d := range readDirs {
		mounts = append(mounts, Mount{Host: d, Target: d, ReadOnly: true})
	}
	for _, ro := range s.Profile.ReadOnly {
		mounts = append(mounts, Mount{Host: ro, Target: ro, ReadOnly: true})
	}
	for _, l := range layered(s.Rules) {
		mounts = append(mounts, Mount{Host: l.Path, Target: l.Path, ReadOnly: l.ReadOnly})
	}
	for _, h := range append([]string{s.RyshDir}, hidden...) {
		if h != "" {
			mounts = append(mounts, Mount{Host: h, Target: h, Hidden: true})
		}
	}

	return &Plan{
		Backend:       BackendMacOS,
		Profile:       s.Profile.Name,
		Argv:          argv,
		Env:           inner,
		InnerEnvNames: envNames(inner),
		Mounts:        mounts,
		Tool:          m.toolPath(),
		SBPL:          sbpl,
	}, nil
}

// isAgentInstallUnderClaude is claude's old self-install location,
// ~/.claude/local — code, not credentials, so it may be re-allowed for reads.
func isAgentInstallUnderClaude(d, home string) bool {
	return within(d, filepath.Join(home, ".claude", "local"))
}

// tempDirs are the per-user temp dirs the agent may write: $TMPDIR (and its
// per-user parent, /var/folders/xx/yyy, which also holds the C/ cache dir).
func tempDirs(env []string) []string {
	for _, kv := range env {
		v, ok := strings.CutPrefix(kv, "TMPDIR=")
		if !ok || !filepath.IsAbs(v) {
			continue
		}
		if r, err := filepath.EvalSymlinks(v); err == nil {
			v = r
		}
		v = filepath.Clean(v)
		if filepath.Base(v) == "T" && strings.HasPrefix(v, "/private/var/folders/") {
			return []string{filepath.Dir(v)}
		}
		return []string{v}
	}
	return nil
}

// Stop has nothing to tear down: sandbox-exec IS the agent process (exec'd in
// place), and it goes when the pane's process does.
func (m *MacOS) Stop(*Plan) error { return nil }
