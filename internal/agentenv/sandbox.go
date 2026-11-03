// SPDX-License-Identifier: Apache-2.0

package agentenv

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/rysh-ai/rysh-cli-code/internal/sandbox"
)

// `rysh exec-clean --sandbox <profile> --rysh-dir <dir> --workspace <dir> -- …`
// (design 066). The same scrub as a plain exec-clean, then the agent is exec()d
// INSIDE the profile's sandbox instead of directly: the backend tool (docker /
// podman / sandbox-exec) replaces this process and runs the agent.
//
// FAIL CLOSED. Every way this can go wrong — a profile that does not exist or
// does not parse, a backend that is not installed, a workspace that is not a
// directory or would expose $HOME — returns sandboxExitCode with a line saying
// the agent was not started. There is no branch that execs the agent itself.

// sandboxExitCode is what a refused sandbox exits with (docker run's own code
// for "the container could not be started").
const sandboxExitCode = 125

// sandboxEnv is what the sandbox launch reads from the host; tests replace it.
type sandboxEnv struct {
	GOOS     string
	HostHome func() (string, error)
	UID, GID func() int
	LookPath sandbox.LookPathFunc
	// StartBridge starts the host end of a network: proxy bridge.
	StartBridge func(dir, upstream string, busPorts []int) (*sandbox.Bridge, error)
}

var sandboxDeps = sandboxEnv{
	GOOS:        runtime.GOOS,
	HostHome:    os.UserHomeDir,
	UID:         os.Getuid,
	GID:         os.Getgid,
	LookPath:    exec.LookPath,
	StartBridge: sandbox.StartBridge,
}

type sandboxRequest struct {
	Profile, RyshDir, Workspace string
}

// runSandboxed prepares the sandbox and execs it. env is already Filter()ed.
func runSandboxed(kind Kind, req sandboxRequest, argv, env []string, stderr io.Writer, execFn ExecFunc) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "rysh: %v (the agent was not started)\n", err)
		return sandboxExitCode
	}
	// Each agent keeps its config (and so its conversations) in its own dir
	// under the sandbox home; the host's dir is never copied whole.
	var cfgDir string
	switch kind {
	case Claude:
		cfgDir = ".claude"
	case Codex:
		cfgDir = ".codex"
	default:
		return fail(fmt.Errorf("sandbox: --sandbox does not support %s", kind))
	}
	prof, err := sandbox.Resolve(req.RyshDir, req.Profile)
	if err != nil {
		return fail(err)
	}
	hostHome, err := sandboxDeps.HostHome()
	if err != nil {
		return fail(fmt.Errorf("sandbox: cannot find your home directory: %w", err))
	}
	ws, err := absClean(req.Workspace)
	if err != nil {
		return fail(err)
	}
	ryshDir, err := absClean(req.RyshDir)
	if err != nil {
		return fail(err)
	}
	backend, err := sandbox.Select(prof, sandboxDeps.GOOS, sandboxDeps.LookPath, sandboxDeps.UID(), sandboxDeps.GID())
	if err != nil {
		return fail(err)
	}
	if err := backend.Available(); err != nil {
		return fail(err)
	}
	// The policy fs rules as the daemon last loaded them (design 068). A
	// missing file, a policy that failed to load, or a rule the sandbox cannot
	// enforce exactly all refuse here.
	rules, err := sandbox.LoadRules(ryshDir)
	if err != nil {
		return fail(err)
	}
	spec := sandbox.Spec{
		Profile:      prof,
		Workspace:    ws,
		Home:         sandbox.HomeDir(ryshDir, prof.Name),
		HostHome:     filepath.Clean(hostHome),
		RyshDir:      ryshDir,
		GitCommonDir: sandbox.GitCommonDir(ws),
		Argv:         argv,
		Env:          env,
		Rules:        rules,
		BusPorts:     sandbox.BusPorts(ryshDir, env),
		ProxyPort:    sandbox.SessionProxyPort(ryshDir, env),
		ID:           newSandboxID(),
	}
	copyConfig := prof.CopyConfig
	if kind == Codex {
		// copy_config names claude's entries; codex gets its own fixed set.
		copyConfig = sandbox.DefaultCopyConfigFor(string(Codex))
		spec.SetEnv = []string{"CODEX_HOME=" + filepath.Join(spec.Home, cfgDir)}
	}
	// Validate before touching the disk: a refused spec creates no home.
	if err := sandbox.CheckSpec(spec); err != nil {
		return fail(err)
	}
	// network: proxy on a backend with its own network namespace: start the
	// host end of the bridge now (macOS reaches the proxy port directly).
	if prof.Proxy && backend.Name() != sandbox.BackendMacOS {
		// The upstream is the session record's governance proxy port (R12),
		// never a port taken from the agent's URL; CheckSpec has checked the
		// URL names exactly it.
		br, err := sandboxDeps.StartBridge(filepath.Join(ryshDir, "sandbox", "run", spec.ID),
			fmt.Sprintf("127.0.0.1:%d", spec.ProxyPort), spec.BusPorts)
		if err != nil {
			return fail(err)
		}
		spec.Bridge = br
	}
	if err := sandbox.PrepareHomeFor(spec.Home, filepath.Join(spec.HostHome, cfgDir), cfgDir, copyConfig); err != nil {
		return fail(err)
	}
	plan, err := backend.Prepare(spec)
	if err != nil {
		return fail(err)
	}
	if err := execFn(plan.Argv[0], plan.Argv, plan.Env); err != nil {
		return fail(fmt.Errorf("sandbox: exec %s: %w", plan.Argv[0], err))
	}
	return 0 // only reached by a test double
}

func absClean(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("sandbox: empty path")
	}
	a, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("sandbox: %s: %w", p, err)
	}
	// Resolve symlinks so the mount/allow paths are the real ones (macOS
	// /var → /private/var); a path that does not resolve is refused later by
	// CheckSpec's directory test, so keep the cleaned form then.
	if r, err := filepath.EvalSymlinks(a); err == nil {
		return r, nil
	}
	return filepath.Clean(a), nil
}

func newSandboxID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("p%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}

// SandboxedTypedLine is TypedLine for a pane that asked for a sandbox. It has
// ONE branch that can start the agent — through exec-clean --sandbox — and on a
// host without the rysh binary it prints why and starts nothing. The plain
// TypedLine's static `env -u … CMD` fallback would run the agent unsandboxed,
// which is exactly what a sandboxed pane must never do.
func SandboxedTypedLine(kind Kind, profile, ryshDir, workspace, cmd string) string {
	bin := Binary()
	msg := "rysh: cannot start the sandboxed agent: the rysh binary is missing on this host (the agent was not started)"
	if bin == "" {
		return "printf '%s\\n' " + quoteAnyShell(msg) + " >&2"
	}
	q := quoteAnyShell(bin)
	return "test -x " + q + " && " + q + " exec-clean --agent " + string(kind) +
		" --sandbox " + quoteAnyShell(profile) +
		" --rysh-dir " + quoteAnyShell(ryshDir) +
		" --workspace " + quoteAnyShell(workspace) +
		" -- " + cmd +
		" || test -x " + q + " || printf '%s\\n' " + quoteAnyShell(msg) + " >&2"
}
