// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// Backend is one way of isolating an agent process. The lifecycle is:
//
//	Available — is the tool there at all (docker/podman, sandbox-exec)?
//	Prepare   — validate the Spec exactly and build the Plan: generated files,
//	            the exec argv, the env and the mounts it exposes. Any doubt is an
//	            error; there is no partial plan.
//	exec      — the caller exec()s Plan.Argv with Plan.Env IN PLACE of the agent
//	            (same pid, Run below). That is also how the pane's PTY is
//	            attached: the backend tool inherits the pane's terminal as its
//	            stdio, and the container backend asks for it with -i -t.
//	Stop      — tear down what the plan started when the pane is closed while
//	            the agent is still running (the container by name).
type Backend interface {
	Name() string
	Available() error
	Prepare(spec Spec) (*Plan, error)
	Stop(plan *Plan) error
}

// Spec is everything a backend needs. Every path is absolute and cleaned.
type Spec struct {
	Profile Profile
	// Workspace is mounted read-write at its own path and is the agent's cwd.
	Workspace string
	// Home is the per-profile sandbox home, HOME inside the sandbox.
	Home string
	// HostHome is the real $HOME: nothing under it is exposed except Workspace,
	// Home and (macOS) the agent's own install directory.
	HostHome string
	// RyshDir is the rysh state directory. It holds secrets and the sandbox
	// profiles themselves, so it is hidden even when it sits inside Workspace —
	// an agent that could edit sandbox.yaml could widen its own next sandbox.
	RyshDir string
	// GitCommonDir, when Workspace is a linked git worktree, is the repository's
	// shared .git directory, which the worktree cannot commit without.
	GitCommonDir string
	// Argv is the agent command line (argv[0] as typed, e.g. "claude").
	Argv []string
	// Env is the environment AFTER agentenv.Filter.
	Env []string
	// Bridge is the host end of the proxy bridge for a `network: proxy` spec
	// on a backend with its own network namespace (bwrap, Linux docker).
	Bridge *Bridge
	// BusPorts are the loopback ports of the rysh session bus (NATS) on this
	// host (BusPorts below). A provider URL naming one of them is refused: the
	// provider port is the one loopback port a network-on macOS sandbox keeps.
	BusPorts []int
	// ProxyPort is the session's governance proxy port from its session record
	// (SessionProxyPort), 0 when the proxy is off. It is the ONLY loopback port
	// a sandbox may reach (R12): a provider URL naming any other loopback port
	// is refused, so a base URL pointing at the web UI or a database never
	// opens that service to the agent.
	ProxyPort int
	// SetEnv are NAME=VALUE pairs set explicitly inside the sandbox, for the
	// agent's own config-dir variable (codex: CODEX_HOME=<home>/.codex). Only
	// names in setEnvAllowed, and only paths under Home: they are visible in
	// argv for the container backend, so they must never carry a secret.
	SetEnv []string
	// Rules are concrete host paths from the policy fs rules (design 068),
	// read by LoadRules. They only restrict what the sandbox already exposes:
	// ReadOnly become read-only, ReadWrite re-open a path inside a read-only
	// one, Deny are hidden.
	Rules PathRules
	// ID names this sandbox instance (the container name suffix).
	ID string
}

// PathRules are resolved, absolute host paths.
type PathRules struct {
	ReadOnly  []string
	ReadWrite []string
	Deny      []string
}

// Mount is one host path the sandbox can see.
type Mount struct {
	Host     string
	Target   string
	ReadOnly bool
	// Hidden is a path masked out (an empty tmpfs / a deny rule).
	Hidden bool
}

// Plan is a prepared sandbox.
type Plan struct {
	Backend string
	Profile string
	// Argv is what rysh exec()s; Argv[0] is an absolute path.
	Argv []string
	// Env is the exec environment of the backend TOOL.
	Env []string
	// InnerEnvNames are the env NAMES visible inside the sandbox (sorted).
	InnerEnvNames []string
	// Mounts are the host paths visible inside (container) or allowed
	// (macOS), plus the hidden ones.
	Mounts []Mount
	// ContainerName is set by the container backend (for Stop).
	ContainerName string
	// Tool is the backend tool's absolute path (docker/podman/sandbox-exec).
	Tool string
	// SBPL is the generated sandbox-exec profile (macOS backend).
	SBPL string
}

// Marker is the value of RYSH_SANDBOX inside a sandbox: "<backend>/<profile>".
func (p *Plan) Marker() string { return p.Backend + "/" + p.Profile }

// MarkerEnv is set inside every sandbox. It is SELF-REPORTED — any process can
// set it — so a record that reads it says so.
const MarkerEnv = "RYSH_SANDBOX"

// innerDropNames never cross into a sandbox even after agentenv.Filter: the
// ssh-agent socket is a credential by another name, and the agent's config-dir
// overrides would point it back at the host's own config (and login).
var innerDropNames = map[string]bool{
	"SSH_AUTH_SOCK":     true,
	"SSH_AGENT_PID":     true,
	"HOME":              true,
	"CLAUDE_CONFIG_DIR": true,
	"CODEX_HOME":        true,
	"GNUPGHOME":         true,
	"KUBECONFIG":        true,
	"DOCKER_CONFIG":     true,
	"DOCKER_HOST":       true,
	MarkerEnv:           true,
}

// proxyDropNames never cross into a `network: proxy` sandbox: every name a
// provider credential arrives in — each name agentenv keeps for claude
// (Bedrock and Vertex included) and for codex, minus the provider URLs and
// CODEX_HOME. agentenv's tests hold the two lists together, so a name added
// to a keep list fails there until it is added here. The governance proxy
// holds the real key and swaps the placeholder for it, so the sandbox needs
// none (R9: the key is injected host-side).
var proxyDropNames = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_SESSION_ACCESS_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
	"CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR", "CLAUDE_CODE_CLIENT_KEY",
	"ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_AWS_API_KEY", "ANTHROPIC_IDENTITY_TOKEN_FILE",
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION",
	"AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_BEARER_TOKEN_BEDROCK", "AWS_SHARED_CREDENTIALS_FILE",
	"GOOGLE_APPLICATION_CREDENTIALS", "OPENAI_API_KEY",
}

// ProxyDropNames returns a copy of proxyDropNames (for agentenv's test).
func ProxyDropNames() []string { return append([]string(nil), proxyDropNames...) }

// proxySwitches send claude to AWS or Google instead of its base URL, so a
// `network: proxy` sandbox (which reaches only the governance proxy) cannot
// serve them and refuses the launch.
var proxySwitches = []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"}

// innerParts is what a backend passes to innerEnv for this spec: extra names
// to drop, and names to set. A proxy-mode spec drops every provider credential
// name and sets the proxy's public placeholder in their place.
func (s Spec) innerParts(extraDrop map[string]bool) (map[string]bool, []string) {
	set := append([]string(nil), s.SetEnv...)
	if !s.Profile.Proxy {
		return extraDrop, set
	}
	drop := map[string]bool{}
	for k, v := range extraDrop {
		drop[k] = v
	}
	for _, n := range proxyDropNames {
		drop[n] = true
	}
	set = append(set, "ANTHROPIC_API_KEY="+config.ProxyDummyKey, "OPENAI_API_KEY="+config.ProxyDummyKey)
	return drop, set
}

// setEnvAllowed are the names Spec.SetEnv may set.
var setEnvAllowed = map[string]bool{"CODEX_HOME": true}

// innerEnv returns the env inside the sandbox: env minus innerDropNames and
// extraDrop, plus set (Spec.SetEnv), HOME and the marker. Order is preserved;
// the set names come last.
func innerEnv(env []string, extraDrop map[string]bool, set []string, home, marker string) []string {
	out := make([]string, 0, len(env)+len(set)+2)
	setNames := map[string]bool{}
	for _, kv := range set {
		n, _, _ := strings.Cut(kv, "=")
		setNames[n] = true
	}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		// DOCKER_* / PODMAN_* configure the container CLIENT (its host, its
		// context, its credentials store); nothing inside needs them.
		if name == "" || innerDropNames[name] || extraDrop[name] || setNames[name] ||
			strings.HasPrefix(name, "DOCKER_") || strings.HasPrefix(name, "PODMAN_") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, set...)
	return append(out, "HOME="+home, MarkerEnv+"="+marker)
}

// isSetEnv reports whether name is one of the spec's SetEnv names.
func isSetEnv(set []string, name string) (string, bool) {
	for _, kv := range set {
		if n, _, _ := strings.Cut(kv, "="); n == name {
			return kv, true
		}
	}
	return "", false
}

// envNames returns the sorted names of env.
func envNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// within reports whether p is base or below it.
func within(p, base string) bool {
	if base == "" {
		return false
	}
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sensitiveHomePaths are always hidden from a sandbox (relative to the real
// $HOME), even when a broader allow would cover them.
var sensitiveHomePaths = []string{
	".ssh", ".aws", ".rysh_env", ".gnupg", ".netrc", ".docker", ".kube",
	".config/gcloud", ".azure", "Library/Keychains",
	".claude", ".claude.json", ".codex",
	// The session-bus token (gap01 slice 3, bustoken.Dir).
	".rysh-run",
}

// SensitivePaths returns the absolute always-hidden paths: those under
// hostHome, plus the per-user runtime dirs a bus token could live in, whatever
// the agent's env says (e20 T1 c).
func SensitivePaths(hostHome string) []string {
	out := make([]string, 0, len(sensitiveHomePaths)+2)
	for _, p := range sensitiveHomePaths {
		out = append(out, filepath.Join(hostHome, p))
	}
	if x := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(x) {
		out = append(out, filepath.Join(filepath.Clean(x), "rysh"))
	}
	if runtime.GOOS == "linux" {
		out = append(out, filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "rysh"))
	}
	return out
}

// CheckSpec is the shared fail-closed validation every backend runs first.
func CheckSpec(s Spec) error {
	if !ValidName(s.Profile.Name) {
		return fmt.Errorf("sandbox: invalid profile name %q", s.Profile.Name)
	}
	if len(s.Argv) == 0 || strings.TrimSpace(s.Argv[0]) == "" {
		return fmt.Errorf("sandbox: no agent command")
	}
	for _, p := range []struct{ what, path string }{
		{"workspace", s.Workspace}, {"sandbox home", s.Home}, {"host home", s.HostHome},
	} {
		if p.path == "" || !filepath.IsAbs(p.path) || filepath.Clean(p.path) != p.path {
			return fmt.Errorf("sandbox: %s %q must be an absolute, clean path", p.what, p.path)
		}
	}
	if fi, err := os.Stat(s.Workspace); err != nil || !fi.IsDir() {
		return fmt.Errorf("sandbox: workspace %s is not a directory", s.Workspace)
	}
	// A workspace that IS $HOME, or contains it, would expose everything the
	// sandbox exists to hide.
	if within(s.HostHome, s.Workspace) {
		return fmt.Errorf("sandbox: workspace %s is (or contains) your home directory — start the agent in a project directory", s.Workspace)
	}
	for _, sp := range SensitivePaths(s.HostHome) {
		if within(s.Workspace, sp) {
			return fmt.Errorf("sandbox: workspace %s is inside %s, which a sandbox never exposes", s.Workspace, sp)
		}
	}
	if deniedByRules(s.Workspace, s.Rules) {
		return fmt.Errorf("sandbox: workspace %s is denied by the policy fs rules", s.Workspace)
	}
	if s.GitCommonDir != "" && deniedByRules(s.GitCommonDir, s.Rules) {
		return fmt.Errorf("sandbox: the repository's .git %s is denied by the policy fs rules", s.GitCommonDir)
	}
	for _, kv := range s.SetEnv {
		n, v, _ := strings.Cut(kv, "=")
		if !setEnvAllowed[n] {
			return fmt.Errorf("sandbox: %s cannot be set inside a sandbox", n)
		}
		if !filepath.IsAbs(v) || filepath.Clean(v) != v || !within(v, s.Home) {
			return fmt.Errorf("sandbox: %s must be a clean path inside the sandbox home", n)
		}
	}
	urls := loopbackProviderURLs(s.Env)
	for _, u := range urls {
		for _, bus := range s.BusPorts {
			if u.port == bus {
				return fmt.Errorf("sandbox: the agent's provider URL names loopback port %d, which is the rysh session bus; the sandbox would let the agent reach it (fix ANTHROPIC_BASE_URL / OPENAI_BASE_URL)", u.port)
			}
		}
		// The one loopback port a sandbox opens is the session's governance
		// proxy (R12). Any other is refused: a base URL naming the web UI or a
		// database would otherwise open that service to the agent.
		if s.ProxyPort == 0 || u.port != s.ProxyPort {
			return fmt.Errorf("sandbox: the agent's provider URL names loopback port %d, which is not this session's governance proxy (%s); a sandbox opens no other loopback port (fix ANTHROPIC_BASE_URL / OPENAI_BASE_URL)", u.port, proxyPortText(s.ProxyPort))
		}
		// The bridge dials, and the forwarder listens on, 127.0.0.1 only.
		if s.Profile.Proxy && u.host != "127.0.0.1" {
			return fmt.Errorf("sandbox: profile %q is network: proxy, which needs the provider URL on 127.0.0.1, not %s", s.Profile.Name, u.host)
		}
	}
	if s.Profile.Proxy && (len(urls) == 0 || s.ProxyPort == 0) {
		return fmt.Errorf("sandbox: profile %q is network: proxy, which needs the rysh governance proxy (a loopback ANTHROPIC_BASE_URL / OPENAI_BASE_URL on its port); turn it on with ##proxy on", s.Profile.Name)
	}
	if s.Profile.Proxy {
		for _, sw := range proxySwitches {
			if v, ok := envValue(s.Env, sw); ok && strings.TrimSpace(v) != "" {
				return fmt.Errorf("sandbox: profile %q is network: proxy, which reaches only the governance proxy; %s sends claude elsewhere, so the agent was not started (unset it, or use another profile)", s.Profile.Name, sw)
			}
		}
	}
	if s.RyshDir != "" && within(s.Workspace, s.RyshDir) {
		return fmt.Errorf("sandbox: workspace %s is inside the rysh directory %s", s.Workspace, s.RyshDir)
	}
	if s.RyshDir != "" && !within(s.Home, s.RyshDir) && within(s.Home, s.Workspace) {
		return fmt.Errorf("sandbox: sandbox home %s must not be inside the workspace", s.Home)
	}
	return nil
}

// HomeDir is the sandbox home for profile under ryshDir.
func HomeDir(ryshDir, profile string) string {
	return filepath.Join(ryshDir, "sandbox", "homes", profile)
}

// PrepareHome creates the sandbox home (0700) and copies the profile's
// copy_config entries from hostConfigDir (the host's ~/.claude) into
// <home>/.claude. Symlinks are never followed or copied — a link is how a
// harmless-looking entry would reach ~/.ssh — and credential files are refused
// by name again here, whatever the profile says.
func PrepareHome(home, hostConfigDir string, copyConfig []string) error {
	return PrepareHomeFor(home, hostConfigDir, ".claude", copyConfig)
}

// DefaultCopyConfigFor is what a profile copies by default for an agent kind:
// claude's instructions and commands, or codex's AGENTS.md and prompts. Never
// a config file (settings.json / config.toml can name a credential helper or
// carry an env block) and never a login file.
func DefaultCopyConfigFor(kind string) []string {
	if kind == "codex" {
		return []string{"AGENTS.md", "prompts"}
	}
	return append([]string(nil), defaultCopyConfig...)
}

// PrepareHomeFor is PrepareHome for any agent: the copy lands in
// <home>/<configDirName> (".claude", ".codex").
func PrepareHomeFor(home, hostConfigDir, configDirName string, copyConfig []string) error {
	if configDirName != filepath.Base(configDirName) || !strings.HasPrefix(configDirName, ".") || len(configDirName) < 2 {
		return fmt.Errorf("sandbox: config dir name %q refused", configDirName)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("sandbox: create home %s: %w", home, err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return fmt.Errorf("sandbox: chmod home %s: %w", home, err)
	}
	dst := filepath.Join(home, configDirName)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return fmt.Errorf("sandbox: create %s: %w", dst, err)
	}
	for _, name := range copyConfig {
		if refusedConfigName(name) || name != filepath.Base(name) {
			return fmt.Errorf("sandbox: copy_config %q refused", name)
		}
		src := filepath.Join(hostConfigDir, name)
		fi, err := os.Lstat(src)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("sandbox: %s: %w", src, err)
		}
		if err := copyTree(src, filepath.Join(dst, name), fi); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string, fi os.FileInfo) error {
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return nil // never followed
	case fi.IsDir():
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
		ents, err := os.ReadDir(src)
		if err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
		for _, e := range ents {
			if refusedConfigName(e.Name()) {
				continue
			}
			cfi, err := os.Lstat(filepath.Join(src, e.Name()))
			if err != nil {
				return fmt.Errorf("sandbox: %w", err)
			}
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), cfi); err != nil {
				return err
			}
		}
		return nil
	case fi.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return fmt.Errorf("sandbox: %w", err)
		}
		return out.Close()
	default:
		return nil // sockets, devices, fifos: never
	}
}

// GitCommonDir returns the shared .git directory when ws is a linked worktree
// (its .git is a file "gitdir: <dir>"), else "". The common dir is read from
// <gitdir>/commondir, as git does.
func GitCommonDir(ws string) string {
	gitdir := gitDirOf(ws)
	if gitdir == "" {
		return "" // a directory (ordinary checkout) or no repo
	}
	common := gitdir
	if c, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		cd := strings.TrimSpace(string(c))
		if !filepath.IsAbs(cd) {
			cd = filepath.Join(gitdir, cd)
		}
		common = cd
	}
	return filepath.Clean(common)
}

// gitMetaNames are the entries of a git directory that make git RUN something
// (hooks; core.hooksPath / core.fsmonitor / filter drivers in the config) or
// that REDIRECT git to another git dir (commondir, gitdir), where an agent
// could have put hooks and a config of its own. An agent that could write them
// would leave code for the user's next git command, which runs outside any
// sandbox (gap01 E2). Everything else in .git — objects, refs, the index —
// stays writable, so the agent can still commit.
var gitMetaNames = []string{"commondir", "config", "config.worktree", "gitdir", "hooks"}

// gitMetaWritable lists the gitMetaNames a backend may create empty to hold
// read-only (a container cannot bind a path that does not exist). commondir
// and gitdir are never created: git refuses an empty one.
var gitMetaCreatable = map[string]bool{"hooks": true, "config.worktree": true}

// GitMetaPaths are the git metadata paths a sandbox holds read-only: those of
// <ws>/.git, of a linked worktree's common dir and own git dir, and of every
// submodule git dir under their modules/ (nested ones too). Absolute, clean.
func GitMetaPaths(ws, gitCommonDir string) ([]string, error) {
	dirs, err := gitMetaDirs(ws, gitCommonDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, dir := range dirs {
		for _, n := range gitMetaNames {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out, nil
}

// gitBaseDirs are <ws>/.git (a dir for an ordinary checkout, a file for a
// linked worktree), the common dir and the worktree's own git dir
// (<common>/worktrees/<name>).
func gitBaseDirs(ws, gitCommonDir string) []string {
	dirs := []string{filepath.Join(ws, ".git")}
	if gitCommonDir == "" {
		return dirs
	}
	dirs = append(dirs, gitCommonDir)
	if gd := gitDirOf(ws); gd != "" && gd != gitCommonDir {
		dirs = append(dirs, gd)
	}
	return dirs
}

// gitMetaDirs are gitBaseDirs plus every existing submodule git dir below them.
func gitMetaDirs(ws, gitCommonDir string) ([]string, error) {
	out, _, err := gitDirsAndModuleParents(ws, gitCommonDir)
	return out, err
}

// gitDirsAndModuleParents returns gitMetaDirs and, apart, the other
// directories the submodule walk entered (see GitModuleParents).
func gitDirsAndModuleParents(ws, gitCommonDir string) (dirs, parents []string, err error) {
	for _, d := range gitBaseDirs(ws, gitCommonDir) {
		mods, walked, err := moduleGitDirs(d)
		if err != nil {
			return nil, nil, err
		}
		dirs = append(dirs, d)
		dirs = append(dirs, mods...)
		parents = append(parents, walked...)
	}
	return dirs, parents, nil
}

// GitModuleParents are the directories between a git dir and its submodule
// git dirs: <git dir>/modules, a "/"-named module's parent dirs, and each
// module's own modules/. They must not be renamed either: renamed away, every
// module below them sits at a path no rule names, where the agent writes its
// hooks and config and then renames it back (round 4). <git dir>/modules is
// listed even when it does not exist (macOS refuses to create it; a bind-mount
// backend skips it).
func GitModuleParents(ws, gitCommonDir string) ([]string, error) {
	_, walked, err := gitDirsAndModuleParents(ws, gitCommonDir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range gitBaseDirs(ws, gitCommonDir) {
		if fi, err := os.Lstat(d); err == nil && !fi.IsDir() {
			continue // a linked worktree's .git pointer file
		}
		walked = append(walked, filepath.Join(d, "modules"))
	}
	for _, d := range walked {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

// maxModuleDirs and maxModuleDepth bound the submodule walk. A walk that
// reaches either bound refuses the launch rather than stopping quietly: an
// agent could otherwise fill modules/ with decoy git dirs that sort first and
// leave the real submodule's metadata writable on the next launch.
const (
	maxModuleDirs  = 256
	maxModuleDepth = 8
	// maxModuleWalkDirs bounds the other directories the walk enters
	// (modules/ dirs, "/"-named modules' parents), each of which is pinned.
	maxModuleWalkDirs = 1024
)

// moduleGitDirs returns the git dirs under gitDir/modules, recursively (a
// module name may contain "/", and a module has modules of its own), and every
// other directory the walk entered. A git dir is recognised by isGitDirMarker.
// Symlinks are never followed.
func moduleGitDirs(gitDir string) (out, walked []string, err error) {
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		ents, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil // nothing below a missing path or a file (a linked worktree's .git)
		}
		if err != nil {
			return unreadableModules(gitDir, err)
		}
		if len(walked) >= maxModuleWalkDirs {
			return fmt.Errorf("sandbox: more than %d directories under %s; the sandbox cannot hold the submodule metadata below them, so the agent was not started — remove the unused ones", maxModuleWalkDirs, filepath.Join(gitDir, "modules"))
		}
		walked = append(walked, dir)
		for _, e := range ents {
			if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if depth >= maxModuleDepth {
				return fmt.Errorf("sandbox: submodule git dirs under %s nest deeper than %d levels; the sandbox cannot hold their metadata, so the agent was not started — remove the extra directories under %s", filepath.Join(gitDir, "modules"), maxModuleDepth, filepath.Join(gitDir, "modules"))
			}
			p := filepath.Join(dir, e.Name())
			marker, err := isGitDirMarker(p)
			if err != nil {
				return unreadableModules(gitDir, err)
			}
			if marker {
				if len(out) >= maxModuleDirs {
					return fmt.Errorf("sandbox: more than %d submodule git dirs under %s; the sandbox cannot hold their metadata, so the agent was not started — remove the unused ones", maxModuleDirs, filepath.Join(gitDir, "modules"))
				}
				out = append(out, p)
				if err := walk(filepath.Join(p, "modules"), depth+1); err != nil {
					return err
				}
				continue
			}
			if err := walk(p, depth+1); err != nil { // a module name with a "/" in it
				return err
			}
		}
		return nil
	}
	if err := walk(filepath.Join(gitDir, "modules"), 0); err != nil {
		return nil, nil, err
	}
	return out, walked, nil
}

// unreadableModules refuses the launch when the walk cannot read part of
// modules/. Skipping it would hold nothing there: an agent that ran chmod 000
// on a module dir in an earlier run restores the mode and writes its hooks.
func unreadableModules(gitDir string, err error) error {
	return fmt.Errorf("sandbox: cannot read the submodule git dirs under %s (%v); the sandbox cannot hold their metadata, so the agent was not started — restore the directory's permissions (chmod u+rwx)", filepath.Join(gitDir, "modules"), err)
}

// isGitDirMarker reports whether dir looks like a git dir: a HEAD or a config
// file. config counts on its own because every sandbox holds it read-only (it
// cannot be deleted from inside), while HEAD can be: a module git dir whose
// HEAD an earlier run removed must still be held on the next launch. Any Lstat
// error other than not-exist is returned, never read as "not a git dir".
func isGitDirMarker(dir string) (bool, error) {
	for _, n := range []string{"HEAD", "config"} {
		fi, err := os.Lstat(filepath.Join(dir, n))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if fi.Mode().IsRegular() {
			return true, nil
		}
	}
	return false, nil
}

// GitPins are the git dirs and pointer files that must not be renamed,
// replaced or rewritten: with .git renamed away its config is an ordinary
// writable file again, and a rewritten `gitdir:` pointer (a linked worktree's
// .git file, a submodule's) sends git to a git dir the agent made. They are
// gitMetaDirs plus each submodule's working-tree .git file inside ws.
func GitPins(ws, gitCommonDir string) ([]string, error) {
	dirs, err := gitMetaDirs(ws, gitCommonDir)
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), dirs...)
	for _, d := range dirs {
		wt := coreWorktree(d)
		if wt == "" || !within(wt, ws) || wt == ws {
			continue
		}
		out = append(out, filepath.Join(wt, ".git"))
	}
	return out, nil
}

// coreWorktree is a git dir's core.worktree, resolved against the git dir,
// or "". Only the [core] section's plain `worktree = <path>` form is read.
func coreWorktree(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return ""
	}
	inCore := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inCore = strings.EqualFold(strings.Trim(line, "[] \t"), "core")
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !inCore || !ok || !strings.EqualFold(strings.TrimSpace(k), "worktree") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if v == "" {
			return ""
		}
		if !filepath.IsAbs(v) {
			v = filepath.Join(gitDir, v)
		}
		return filepath.Clean(v)
	}
	return ""
}

// regexQuoteSBPL quotes a path for an SBPL regex literal.
func regexQuoteSBPL(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// gitDirOf is the "gitdir:" a linked worktree's .git file points at, or "".
func gitDirOf(ws string) string {
	data, err := os.ReadFile(filepath.Join(ws, ".git"))
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(ws, gitdir)
	}
	return filepath.Clean(gitdir)
}

// providerURLNames are the env names whose URL is the agent's own way to its
// model (the governance proxy when rysh runs one). A loopback URL here opens
// exactly that port inside a network-on sandbox; nothing else on loopback.
var providerURLNames = []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"}

// loopbackURL is a provider URL's loopback host and explicit port.
type loopbackURL struct {
	host string
	port int
}

// loopbackProviderURLs returns the agent's provider URLs that name a loopback
// host with an explicit port. A URL that is not loopback, or has no explicit
// port, is not one.
func loopbackProviderURLs(env []string) []loopbackURL {
	var out []loopbackURL
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		wanted := false
		for _, n := range providerURLNames {
			wanted = wanted || name == n
		}
		if !wanted {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(val))
		if err != nil {
			continue
		}
		switch u.Hostname() {
		case "127.0.0.1", "localhost", "::1":
		default:
			continue
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		out = append(out, loopbackURL{host: u.Hostname(), port: port})
	}
	return out
}

// LoopbackProxyPorts returns the loopback ports the agent's provider URLs name
// (sorted, unique). CheckSpec accepts a spec only when this is empty or just
// the session's ProxyPort.
func LoopbackProxyPorts(env []string) []int {
	seen := map[int]bool{}
	var out []int
	for _, u := range loopbackProviderURLs(env) {
		if !seen[u.port] {
			seen[u.port] = true
			out = append(out, u.port)
		}
	}
	sort.Ints(out)
	return out
}

func proxyPortText(port int) string {
	if port == 0 {
		return "the proxy is off"
	}
	return fmt.Sprintf("port %d", port)
}

// envValue returns name's value in env.
func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// SessionProxyPort returns the governance proxy port on the session record
// of the pane's session (<ryshDir>/sessions/<RYSH_SESSION>.json, the name
// sanitised as the session store does), or 0 when the record is missing,
// unreadable or has no proxy. An unset or empty RYSH_SESSION is 0 too, never
// the "default" record: every rysh pane has RYSH_SESSION, and a guess could
// open another session's port. RYSH_SESSION_DIR is not read.
func SessionProxyPort(ryshDir string, env []string) int {
	name, _ := envValue(env, "RYSH_SESSION")
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	name = strings.ReplaceAll(name, string(filepath.Separator), "-")
	if ryshDir == "" || name == "." || name == ".." {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(ryshDir, "sessions", name+".json"))
	if err != nil {
		return 0
	}
	var rec struct {
		ProxyPort int `json:"proxy_port"`
	}
	if json.Unmarshal(data, &rec) != nil || rec.ProxyPort <= 0 || rec.ProxyPort > 65535 {
		return 0
	}
	return rec.ProxyPort
}

// defaultBusPort is the session bus port when nothing configures another
// (config's NATS default, nats://localhost:4222).
const defaultBusPort = 4222

// BusPorts returns the session bus ports a sandbox must never be let through
// to: every nats_port in <ryshDir>/sessions/*.json, RYSH_NATS_PORT from env,
// and the default port. Sorted, unique. Unreadable records are skipped: the
// list only ever ADDS refusals.
func BusPorts(ryshDir string, env []string) []int {
	seen := map[int]bool{defaultBusPort: true}
	if ryshDir != "" {
		files, _ := filepath.Glob(filepath.Join(ryshDir, "sessions", "*.json"))
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var rec struct {
				NATSPort int `json:"nats_port"`
			}
			if json.Unmarshal(data, &rec) == nil && rec.NATSPort > 0 && rec.NATSPort <= 65535 {
				seen[rec.NATSPort] = true
			}
		}
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "RYSH_NATS_PORT="); ok {
			if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && p > 0 && p <= 65535 {
				seen[p] = true
			}
		}
	}
	out := make([]int, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// LookPathFunc resolves a tool on PATH (exec.LookPath in production).
type LookPathFunc func(string) (string, error)

// Select picks the backend a profile asks for. auto is macOS sandbox-exec on
// darwin and the container backend elsewhere. It never substitutes one backend
// for another the profile named, and never returns "none".
func Select(p Profile, goos string, lookPath LookPathFunc, uid, gid int) (Backend, error) {
	if goos == "" {
		goos = runtime.GOOS
	}
	name := p.Backend
	if name == BackendAuto || name == "" {
		name = BackendContainer
		switch goos {
		case "darwin":
			name = BackendMacOS
		case "linux":
			// An OS sandbox needs no daemon and no image: prefer it — but
			// only with the network OFF. With it on, bwrap shares the host's
			// network namespace (host loopback, the session bus), which a
			// container on its own network does not.
			if lp, err := lookPath("bwrap"); err == nil && lp != "" && !p.Network { // off or proxy
				name = BackendBwrap
			}
		}
	}
	switch name {
	case BackendContainer:
		if p.Proxy && (goos == "darwin" || goos == "windows") {
			// Measured on colima (TestContainerUnixSocketCrossesE2E): a host
			// unix socket does not cross a VM-backed docker's shared folder,
			// so the proxy bridge cannot reach into the container.
			return nil, fmt.Errorf("sandbox: profile %q: network: proxy with the container backend needs the proxy's unix socket inside the container, and this %s docker runs in a VM whose shared folders do not carry unix sockets; use the macos backend here", p.Name, goos)
		}
		return &Container{LookPath: lookPath, UID: uid, GID: gid}, nil
	case BackendMacOS:
		if goos != "darwin" {
			return nil, fmt.Errorf("sandbox: profile %q asks for the macos backend, which needs macOS (this is %s)", p.Name, goos)
		}
		return &MacOS{LookPath: lookPath}, nil
	case BackendBwrap:
		if goos != "linux" {
			return nil, fmt.Errorf("sandbox: profile %q asks for the bwrap backend, which needs Linux (this is %s)", p.Name, goos)
		}
		if p.Network {
			return nil, errBwrapNetworkOn(p.Name)
		}
		return &Bwrap{LookPath: lookPath}, nil
	}
	return nil, fmt.Errorf("sandbox: unknown backend %q", name)
}
