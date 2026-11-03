// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ungoverned-CLI detection and per-CLI adapters (design 022 §4.4).
//
// Design 001 §2 listed "CLIs that ignore base-URL env vars" as a non-goal and
// §7 promised an unproxied warning that was never built. The gap matters
// because the failure is SILENT: the pane keeps working, the CLI talks to the
// provider directly, and nothing in rysh says the traffic escaped. A user
// reasonably reads "governance proxy: ON" as "this pane is governed".
//
// Two halves:
//
//  1. DETECT. When a known agent CLI is the pane's foreground process and no
//     governed request arrives within a grace window, warn once. This is
//     NEGATIVE evidence and is treated as such: an idle CLI is indistinguishable
//     from an escaped one, so it warns and never blocks.
//
//  2. CLOSE. Env injection alone does not govern every CLI, and we already knew
//     it: the wire harness has to pin a model_provider and a scratch CODEX_HOME
//     to stop a ChatGPT-authenticated machine falling back to ungoverned egress.
//     That knowledge lived only in cmd/wire-harness. It lives here now, so the
//     harness, `##proxy check` and pane spawn all read it from one place and
//     cannot drift apart.

// CLIProfile is what rysh knows about one third-party agent CLI.
type CLIProfile struct {
	// Binary is the executable name as it appears in the process table.
	Binary string
	// Dialect is which proxy dialect this CLI's traffic speaks.
	Dialect string
	// BaseURLEnv is the environment variable it reads its endpoint from.
	BaseURLEnv string
	// KeyEnv is the environment variable it reads its credential from.
	KeyEnv string
	// EnvIsEnough reports whether exporting BaseURLEnv actually governs this
	// CLI. False means the CLI needs an adapter (see AdaptSpawn) and that
	// base-URL injection alone will silently leave it ungoverned.
	EnvIsEnough bool
	// Note explains a false EnvIsEnough, and is what the pane warning says.
	Note string
	// Install is where this CLI's genuine files live. A process is identified
	// as this CLI only from these locations; see IdentifyCLI.
	Install InstallLocations

	// probeArgs builds the argv for a one-shot governed probe (`##proxy check`
	// and the wire harness). scratch is a caller-owned temp dir.
	probeArgs func(bin, baseURL, prompt, scratch string) []string
	// adaptEnv returns the extra environment that forces this CLI through
	// baseURL. dir is a rysh-owned directory the adapter may populate.
	adaptEnv func(baseURL, dir string) ([]string, error)
}

// profiles is the known-CLI table, keyed by binary name.
var profiles = map[string]CLIProfile{
	"claude": {
		Binary: "claude", Dialect: "anthropic",
		BaseURLEnv: "ANTHROPIC_BASE_URL", KeyEnv: "ANTHROPIC_API_KEY",
		EnvIsEnough: true,
		Install: InstallLocations{
			Executables: concat(
				// The native installer. Source: https://code.claude.com/docs/en/setup
				// ("Auto-updates": "the native installer manages the launcher at
				// ~/.local/bin/claude as a symlink into ~/.local/share/claude/versions/").
				[]string{"~/.local/share/claude/versions/*"},
				// Homebrew casks claude-code and claude-code@latest. Source: the same
				// page ("Homebrew" tab), and each cask's `binary "claude"` artifact:
				// https://formulae.brew.sh/api/cask/claude-code.json and
				// https://formulae.brew.sh/api/cask/claude-code@latest.json.
				brewCask("claude-code", "claude"),
				brewCask("claude-code@latest", "claude"),
				// npm: the package installs the native binary at bin/claude.exe.
				// Source: the same page ("Install with npm"), the package's
				// `"bin": {"claude": "bin/claude.exe"}` and its postinstall
				// install.cjs (dest bin/claude.exe):
				// https://unpkg.com/@anthropic-ai/claude-code/package.json.
				npmGlobal("@anthropic-ai/claude-code/bin/claude.exe"),
				// npm: the source of that hard link, the platform package's own
				// binary (see claudeNpmPlatformBinaries).
				claudeNpmPlatformBinaries(),
			),
		},
		probeArgs: func(bin, _, prompt, _ string) []string {
			return []string{bin, "--print", prompt}
		},
	},
	"codex": {
		Binary: "codex", Dialect: "openai",
		BaseURLEnv: "OPENAI_BASE_URL", KeyEnv: "OPENAI_API_KEY",
		EnvIsEnough: false,
		Note: "codex can fall back to ChatGPT auth and egress directly; " +
			"OPENAI_BASE_URL alone does not govern it",
		Install: InstallLocations{
			Executables: concat(
				// Homebrew cask codex. Source: https://github.com/openai/codex
				// (README, "brew install --cask codex") and the cask's
				// `binary "bin/codex"` artifact: https://formulae.brew.sh/api/cask/codex.json.
				brewCask("codex", "bin/codex"),
			),
			// npm: node runs the package's bin script. Source: the same README
			// ("npm install -g @openai/codex") and the package's
			// `"bin": {"codex": "bin/codex.js"}`: https://unpkg.com/@openai/codex/package.json.
			Scripts: npmGlobal("@openai/codex/bin/codex.js"),
		},
		probeArgs: func(bin, baseURL, prompt, scratch string) []string {
			// Pinning a custom provider AT the proxy URL (rather than only
			// exporting OPENAI_BASE_URL) is what guarantees no fallback egress
			// on a ChatGPT-authenticated machine.
			return []string{bin, "exec",
				"--skip-git-repo-check",
				"-c", "model_provider=rysh",
				"-c", "model_providers.rysh.name=rysh governance proxy",
				"-c", "model_providers.rysh.base_url=" + baseURL + "/v1",
				"-c", "model_providers.rysh.env_key=OPENAI_API_KEY",
				"-c", "model=gpt-4o",
				prompt}
		},
		adaptEnv: codexAdaptEnv,
	},
	"aider": {
		Binary: "aider", Dialect: "openai",
		BaseURLEnv: "OPENAI_BASE_URL", KeyEnv: "OPENAI_API_KEY",
		EnvIsEnough: true,
		Install: InstallLocations{
			Scripts: []string{
				// uv tool (aider-install, the install.sh one-liner and `uv tool
				// install aider-chat`). Source: https://aider.chat/docs/install.html,
				// and uv's tools directory and per-tool environment:
				// https://docs.astral.sh/uv/reference/storage/ ("tools are installed
				// in a tools/ subdirectory ... e.g., ~/.local/share/uv/tools") and
				// https://docs.astral.sh/uv/concepts/tools/ ("Tool environments": a
				// virtual environment per tool). A virtual environment keeps its
				// scripts in bin/ on POSIX: https://docs.python.org/3/library/venv.html.
				"~/.local/share/uv/tools/*/bin/aider",
				// pipx (`pipx install aider-chat`). Source: the aider page, and
				// https://pipx.pypa.io/latest/reference/environment-variables.html
				// (PIPX_HOME: Linux ~/.local/share/pipx, macOS
				// ~/Library/Application Support/pipx, legacy ~/.local/pipx;
				// PIPX_LOCAL_VENVS is $PIPX_HOME/venvs).
				"~/.local/share/pipx/venvs/*/bin/aider",
				"~/Library/Application Support/pipx/venvs/*/bin/aider",
				"~/.local/pipx/venvs/*/bin/aider",
			},
		},
		probeArgs: func(bin, _, prompt, _ string) []string {
			return []string{bin, "--no-git", "--yes", "--message", prompt}
		},
	},
	"openclaw": {
		Binary: "openclaw", Dialect: "anthropic",
		BaseURLEnv: "ANTHROPIC_BASE_URL", KeyEnv: "ANTHROPIC_API_KEY",
		EnvIsEnough: true,
		Install: InstallLocations{
			// npm, the default method of the installer: node runs the package's
			// bin script. Source: https://docs.openclaw.ai/install/installer.md
			// ("npm method (default): global npm install") and the package's
			// `"bin": {"openclaw": "openclaw.mjs"}`: https://unpkg.com/openclaw/package.json.
			// install-cli.sh's ~/.openclaw prefix is left out: its package
			// directory is not documented.
			Scripts: npmGlobal("openclaw/openclaw.mjs"),
		},
		probeArgs: func(bin, _, prompt, _ string) []string {
			return []string{bin, "--print", prompt}
		},
	},
	"gemini": {
		Binary: "gemini", Dialect: "gemini",
		BaseURLEnv: "GEMINI_BASE_URL", KeyEnv: "GEMINI_API_KEY",
		EnvIsEnough: true,
		Install: InstallLocations{
			Scripts: concat(
				// npm: node runs the package's bin script. Source:
				// https://github.com/google-gemini/gemini-cli (README, "npm install -g
				// @google/gemini-cli") and the package's
				// `"bin": {"gemini": "bundle/gemini.js"}`: https://unpkg.com/@google/gemini-cli/package.json.
				npmGlobal("@google/gemini-cli/bundle/gemini.js"),
				// Homebrew formula gemini-cli ("brew install gemini-cli", same
				// README): std_npm_args installs "in npm's global layout under
				// libexec" (https://docs.brew.sh/Language-Specific-Formulae), and the
				// formula reads libexec/"lib/node_modules/@google/gemini-cli"
				// (https://github.com/Homebrew/homebrew-core/blob/bf15382d54581a9a5f9836fbe141cdd1b57eff67/Formula/g/gemini-cli.rb).
				brewCellar("gemini-cli", "libexec/lib/node_modules/@google/gemini-cli/bundle/gemini.js"),
			),
		},
		probeArgs: func(bin, _, prompt, _ string) []string {
			return []string{bin, "-p", prompt}
		},
	},
}

// ProfileFor returns what rysh knows about a binary. The lookup tolerates a
// path and a .exe suffix, because a process table reports whatever was exec'd.
func ProfileFor(binary string) (CLIProfile, bool) {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.TrimSpace(binary))), ".exe")
	p, ok := profiles[name]
	return p, ok
}

// InstallLocations lists where an allowed CLI's genuine files live, as
// filepath.Match patterns over absolute, symlink-free paths. A leading "~/" is
// the user's home directory.
type InstallLocations struct {
	// Executables match the real executable of a CLI that runs as a binary.
	Executables []string
	// Scripts match the real path of the entry script of a CLI that runs
	// under an interpreter.
	Scripts []string
}

// The standard installers' documented roots. A location is "known" because
// it is documented, so every root and every per-CLI entry cites its source.
var (
	// brewPrefixes are Homebrew's default prefixes. Source: Homebrew manpage,
	// `brew --prefix`, https://docs.brew.sh/Manpage ("macOS ARM: /opt/homebrew",
	// "macOS Intel: /usr/local", "Linux: /home/linuxbrew/.linuxbrew").
	brewPrefixes = []string{"/opt/homebrew", "/usr/local", "/home/linuxbrew/.linuxbrew"}

	// npmPrefixes are npm's global prefixes for a standard node:
	//   - /usr/local, npm's default. Source: https://docs.npmjs.com/cli/v10/configuring-npm/folders
	//     ("prefix Configuration": "On most systems, this is /usr/local"; the
	//     prefix is one level above the node binary, so Homebrew's node gives a
	//     Homebrew prefix).
	//   - The Homebrew prefixes, for Homebrew's node (as above).
	//   - nvm's per-version prefix. Source: https://github.com/nvm-sh/nvm
	//     (README, macOS notes: "~/.nvm/versions/node/vX.X.X/lib/node_modules/*").
	npmPrefixes = []string{"/usr/local", "/opt/homebrew", "/home/linuxbrew/.linuxbrew", "~/.nvm/versions/node/*"}
)

// npmGlobal lists rel under every npm global prefix. Global packages live in
// {prefix}/lib/node_modules. Source: https://docs.npmjs.com/cli/v10/configuring-npm/folders
// ("Node Modules": "Global installs on Unix systems go to {prefix}/lib/node_modules").
func npmGlobal(rel string) []string {
	out := make([]string, 0, len(npmPrefixes))
	for _, p := range npmPrefixes {
		out = append(out, p+"/lib/node_modules/"+rel)
	}
	return out
}

// brewCask lists rel inside every version of a Homebrew cask. A cask is staged
// at $(brew --caskroom)/<token>/<version>. Source: https://docs.brew.sh/Cask-Cookbook
// (caveats: "the staged location for this cask, including version number").
// The Caskroom is HOMEBREW_PREFIX/"Caskroom". Source:
// https://docs.brew.sh/rubydoc/Cask/Caskroom.html (Caskroom.path).
func brewCask(token, rel string) []string {
	out := make([]string, 0, len(brewPrefixes))
	for _, p := range brewPrefixes {
		out = append(out, p+"/Caskroom/"+token+"/*/"+rel)
	}
	return out
}

// brewCellar lists rel inside every version of a Homebrew formula. The Cellar
// is $(brew --prefix)/Cellar. Source: https://docs.brew.sh/Manpage (`--cellar`).
func brewCellar(formula, rel string) []string {
	out := make([]string, 0, len(brewPrefixes))
	for _, p := range brewPrefixes {
		out = append(out, p+"/Cellar/"+formula+"/*/"+rel)
	}
	return out
}

// claudeNpmPlatforms are the darwin and linux keys of install.cjs's PLATFORMS
// table, each the suffix of a platform package
// "@anthropic-ai/claude-code-<key>" whose binary is "claude". The -android keys
// are left out: getPlatformKey returns them only when process.platform is
// "android". Source: https://unpkg.com/@anthropic-ai/claude-code/install.cjs
// (PACKAGE_PREFIX, BINARY_NAME, PLATFORMS, getPlatformKey).
var claudeNpmPlatforms = []string{
	"darwin-arm64", "darwin-x64",
	"linux-x64", "linux-arm64", "linux-x64-musl", "linux-arm64-musl",
}

// claudeNpmPlatformBinaries lists where npm puts the platform package's
// binary that install.cjs hard-links to bin/claude.exe. install.cjs finds it
// with require.resolve, so its directory is where Node resolves the package
// from the claude-code package. For a global npm install, that is one of two
// places:
//   - nested: {prefix}/lib/node_modules/@anthropic-ai/claude-code/node_modules/<pkg>
//   - hoisted: {prefix}/lib/node_modules/<pkg>
//
// Sources: install.cjs (above: `require.resolve(info.pkg + '/package.json')`,
// `dest = path.join(__dirname, 'bin', 'claude.exe')`);
// https://docs.npmjs.com/cli/v10/configuring-npm/folders ("Global installs on
// Unix systems go to {prefix}/lib/node_modules"); and
// https://nodejs.org/api/modules.html ("Loading from node_modules folders":
// the module's own node_modules first, then each parent directory's). Node's
// other lookup places (NODE_PATH, $HOME/.node_modules, $HOME/.node_libraries,
// {prefix}/lib/node) and any node_modules above the prefix are not npm
// global-install outputs, so they are not listed.
func claudeNpmPlatformBinaries() []string {
	var out []string
	for _, p := range claudeNpmPlatforms {
		pkg := "@anthropic-ai/claude-code-" + p
		out = append(out, npmGlobal("@anthropic-ai/claude-code/node_modules/"+pkg+"/claude")...)
		out = append(out, npmGlobal(pkg+"/claude")...)
	}
	return out
}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// IdentifyCLI names the allowed agent CLI a process is. realExe is the
// process's real executable as the kernel reports it (darwin proc_pidpath,
// Linux /proc/<pid>/exe), and argv its arguments. It returns the profile's own
// binary name, so every notice names the CLI the way `##proxy check` expects.
//
// The real executable decides. A process's kernel name, the path it was
// exec'd through and its argv are all chosen by whoever launched it, so none
// of them can make a process count as a CLI. That works in both directions:
// an `exec -a claude` decoy is not claude, and the real claude is still claude
// when it is reached through another name. The native installer's
//
//	~/.local/bin/claude -> ~/.local/share/claude/versions/<version>
//
// is identified by the version file it resolves to (F-15).
//
// When the real executable is an interpreter (node, python3, …), the CLI is
// the script it runs. argv[1] is resolved to a real file, and that file must
// be at one of the CLI's install locations. A relative or unresolvable script
// is not identified.
//
// Strict mode governs the allowed CLIs at their known install locations; it is
// not an anti-evasion boundary against the user's own machine. ok is false for
// anything else, including a process whose executable cannot be read, and a
// copy outside the known install locations is unknown (F-13): only a known
// agent CLI is watched (design 022 §4.4).
func IdentifyCLI(realExe string, argv []string) (string, bool) {
	return identifyCLI(realExe, argv, installedRoot)
}

// rootFunc maps an absolute install-location pattern to the pattern that is
// matched, or reports false to skip it.
type rootFunc func(pattern string) (string, bool)

// installedRoot is the only rootFunc outside tests: every location is matched
// where it is listed. Nothing can replace it in a built binary; tests pass
// their own rootFunc to identifyCLI directly.
func installedRoot(pattern string) (string, bool) { return pattern, true }

// identifyCLI is IdentifyCLI with the absolute install locations mapped
// through root.
func identifyCLI(realExe string, argv []string, root rootFunc) (string, bool) {
	exe := realPath(realExe)
	if exe == "" {
		return "", false
	}
	if name, ok := matchInstall(exe, func(l InstallLocations) []string { return l.Executables }, root); ok {
		return name, true
	}
	if !isInterpreter(exe) || len(argv) < 2 || !filepath.IsAbs(argv[1]) {
		return "", false
	}
	script := realPath(argv[1])
	if script == "" {
		return "", false
	}
	return matchInstall(script, func(l InstallLocations) []string { return l.Scripts }, root)
}

// matchInstall reports the profile whose install locations (as picked) contain
// path. "~/" locations are expanded against the home directory; absolute ones
// go through root.
func matchInstall(path string, pick func(InstallLocations) []string, root rootFunc) (string, bool) {
	for _, name := range KnownCLIs() {
		p := profiles[name]
		for _, pattern := range pick(p.Install) {
			candidates := expandHome(pattern)
			if !strings.HasPrefix(pattern, "~/") {
				mapped, ok := root(pattern)
				if !ok {
					continue
				}
				candidates = []string{mapped}
			}
			for _, abs := range candidates {
				if ok, _ := filepath.Match(abs, path); ok {
					return p.Binary, true
				}
			}
		}
	}
	return "", false
}

// expandHome returns pattern with a leading "~/" replaced by the home
// directory, both as given and with its symlinks resolved, so it compares
// with a symlink-free path either way. Without a home it yields nothing.
func expandHome(pattern string) []string {
	rest, ok := strings.CutPrefix(pattern, "~/")
	if !ok {
		return []string{pattern}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	out := []string{filepath.Join(home, rest)}
	if real, err := filepath.EvalSymlinks(home); err == nil && real != home {
		out = append(out, filepath.Join(real, rest))
	}
	return out
}

// realPath returns path made absolute and symlink-free, or "" if it does not
// resolve to an existing file.
func realPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return real
}

// isInterpreter reports whether a real executable is a runtime that runs a
// CLI from a script, so the script is the program's identity.
func isInterpreter(exe string) bool {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(exe)), ".exe")
	switch name {
	case "node", "nodejs", "bun", "deno", "python", "python3":
		return true
	}
	return strings.HasPrefix(name, "python3.")
}

// KnownCLIs lists the binaries rysh recognises, sorted for stable output.
func KnownCLIs() []string {
	out := make([]string, 0, len(profiles))
	for name := range profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ProbeCommand builds the argv and environment for a one-shot run of this CLI
// against baseURL. Used by `##proxy check` and by cmd/wire-harness, so the
// answer to "how do you force this CLI through a base URL" has ONE home.
//
// dummyKey is what lands in the CLI's credential variable: the probe must never
// need a working key, since the point is to observe the wire, not to complete.
func (p CLIProfile) ProbeCommand(bin, baseURL, prompt, scratch, dummyKey string) ([]string, []string, error) {
	argv := p.probeArgs(bin, baseURL, prompt, scratch)
	env := []string{
		p.BaseURLEnv + "=" + baseURL,
		p.KeyEnv + "=" + dummyKey,
	}
	if p.adaptEnv != nil {
		extra, err := p.adaptEnv(baseURL, scratch)
		if err != nil {
			return nil, nil, err
		}
		env = append(env, extra...)
	}
	return argv, env, nil
}

// AdaptSpawn returns the extra environment that forces this CLI through baseURL
// at pane-spawn time, where rysh controls the environment but NOT the argv.
//
// dir is a rysh-owned directory the adapter may write into. Returns nil when
// the CLI needs no adaptation.
func (p CLIProfile) AdaptSpawn(baseURL, dir string) ([]string, error) {
	if p.adaptEnv == nil {
		return nil, nil
	}
	return p.adaptEnv(baseURL, dir)
}

// codexAdaptEnv points CODEX_HOME at a rysh-owned directory holding a config
// that pins the provider at the proxy.
//
// The directory deliberately contains NO auth.json. That is the whole
// mechanism: with a ChatGPT session available, codex can egress directly and
// the pane is silently ungoverned — which is exactly what the wire harness had
// to defeat. A governed pane therefore uses the API-key path through the proxy
// and does not use an existing ChatGPT login. That is a real behavioural change,
// which is why the adapter is opt-in ([proxy] adapters) rather than automatic.
func codexAdaptEnv(baseURL, dir string) ([]string, error) {
	if dir == "" {
		return nil, fmt.Errorf("codex adapter: no directory to write config into")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("codex adapter: %w", err)
	}
	cfg := "" +
		"# Written by rysh (design 022 §4.4). Points codex at the governance proxy.\n" +
		"# This home deliberately carries no auth.json: a ChatGPT session would let\n" +
		"# codex egress directly, leaving the pane silently ungoverned.\n" +
		"model_provider = \"rysh\"\n\n" +
		"[model_providers.rysh]\n" +
		"name = \"rysh governance proxy\"\n" +
		"base_url = \"" + baseURL + "/v1\"\n" +
		"env_key = \"OPENAI_API_KEY\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		return nil, fmt.Errorf("codex adapter: %w", err)
	}
	return []string{"CODEX_HOME=" + dir}, nil
}

// ---- governance observation ----

// GraceWindow is how long a known CLI may run without a governed request before
// rysh says so. Long enough that a cold start is not accused, short enough that
// an escaped CLI is reported while the user is still watching.
const GraceWindow = 20 * time.Second

// govWatch tracks, per pane, when a known CLI came to the foreground and
// whether a governed request has been seen since.
//
// The mutex guards only this map: same documented data-plane exception as the
// audit ring and the budget cache.
type govWatch struct {
	mu      sync.Mutex
	started map[string]watchEntry // paneID → current observation
	seen    map[string]time.Time  // paneID → last governed request
}

type watchEntry struct {
	binary string
	at     time.Time
	warned bool
}

func newGovWatch() *govWatch {
	return &govWatch{
		started: map[string]watchEntry{},
		seen:    map[string]time.Time{},
	}
}

// noteGoverned records that a pane's traffic reached the proxy. This is the
// POSITIVE evidence that clears any pending suspicion.
func (g *govWatch) noteGoverned(paneID string) {
	if g == nil || paneID == "" {
		return
	}
	g.mu.Lock()
	g.seen[paneID] = time.Now()
	if e, ok := g.started[paneID]; ok {
		e.warned = true // governed: never warn about this run
		g.started[paneID] = e
	}
	g.mu.Unlock()
}

// noteForeground records that binary became the pane's foreground process.
// A change resets the observation, so each run of a CLI is judged on its own.
//
// Only a known agent CLI is watched (design 022 §4.4). Anything else — a
// build, a test run, `ssh`, an editor, `sleep 120` — is treated exactly like
// the shell being back in front, because strict acts on the ABSENCE of
// governed traffic and a program that never talks to a provider is always
// absent (F-13). The caller must pass the identity IdentifyCLI gives from the
// real executable, never a name the process reports for itself (F-15). Strict
// mode therefore governs the allowed CLIs at their known install locations; it
// is not an anti-evasion boundary against the user's own machine, and a copy
// outside those locations is unknown, so it is not watched.
func (g *govWatch) noteForeground(paneID, binary string, now time.Time) {
	if g == nil || paneID == "" {
		return
	}
	if p, ok := ProfileFor(binary); ok {
		binary = p.Binary
	} else {
		binary = ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if binary == "" {
		delete(g.started, paneID)
		return
	}
	if e, ok := g.started[paneID]; ok && e.binary == binary {
		return // same program still running
	}
	g.started[paneID] = watchEntry{binary: binary, at: now}
}

// forget drops every observation for a pane that has gone away — both halves.
//
// noteForeground(paneID, "") alone was not enough: it clears `started` but
// leaves `seen`, so a long-lived daemon accumulates one timestamp per pane it
// has ever proxied, and a recycled pane ID inherits a governed state it did
// not earn.
func (g *govWatch) forget(paneID string) {
	if g == nil || paneID == "" {
		return
	}
	g.mu.Lock()
	delete(g.started, paneID)
	delete(g.seen, paneID)
	g.mu.Unlock()
}

// dueWarning reports a pane whose known CLI has run past the grace window with
// no governed request, and latches so the warning is emitted exactly once per
// run of that program.
func (g *govWatch) dueWarning(paneID string, now time.Time) (string, bool) {
	if g == nil || paneID == "" {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.started[paneID]
	if !ok || e.warned || now.Sub(e.at) < GraceWindow {
		return "", false
	}
	if last, ok := g.seen[paneID]; ok && last.After(e.at) {
		return "", false // governed since this program started
	}
	e.warned = true
	g.started[paneID] = e
	return e.binary, true
}

// state reports how a pane looks right now, for ##proxy status.
func (g *govWatch) state(paneID string, now time.Time) string {
	if g == nil {
		return "unknown"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if last, ok := g.seen[paneID]; ok && now.Sub(last) < 5*time.Minute {
		return "governed"
	}
	e, ok := g.started[paneID]
	if !ok {
		return "idle"
	}
	if now.Sub(e.at) >= GraceWindow {
		return "unproxied?"
	}
	return "starting"
}

// StrictBlockNotice is the pane line for a CLI that strict mode is about to
// stop. It names what is being stopped, why, and how to get the deterministic
// answer — because strict mode acts on negative evidence and will occasionally
// stop something innocent.
func StrictBlockNotice(binary string) string {
	env := "the base-URL env var"
	if p, ok := ProfileFor(binary); ok {
		env = p.BaseURLEnv
	}
	return fmt.Sprintf(
		"[proxy] strict: stopping '%s' — it ran past the grace window with no "+
			"governed request, so its provider traffic is not being redacted, "+
			"metered or capped. Confirm with `##proxy check %s`, or clear "+
			"[proxy] strict / policy proxy.strict to go back to warning only. "+
			"(It ignores %s.)\n", binary, binary, env)
}

// UnproxiedWarning is the pane line for a CLI that looks ungoverned. It says
// what was observed and what to do, and is explicit that this is a suspicion —
// an idle CLI looks the same from here.
func UnproxiedWarning(binary string) string {
	p, ok := ProfileFor(binary)
	detail := ""
	if ok && p.Note != "" {
		detail = " " + p.Note + "."
	}
	env := "the base-URL env var"
	if ok {
		env = p.BaseURLEnv
	}
	return fmt.Sprintf(
		"[proxy] warning: '%s' has been running with no governed request — "+
			"it may be ignoring %s.%s Confirm with `##proxy check %s`; "+
			"see docs/proxy-compat-matrix.md.\n", binary, env, detail, binary)
}
