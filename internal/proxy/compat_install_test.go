// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The standard installers' install locations (WO-7 amendment 5). Each layout
// below mirrors a documented real target with a FAKE stand-in file, reached
// through a bin-name symlink the way the installer links it. The absolute
// roots (/opt/homebrew, /usr/local, /home/linuxbrew/.linuxbrew) are mapped
// into temp dirs through identifyCLI's root argument; "~/" layouts sit under
// a fake HOME.

// testRoots maps the absolute install roots into temp dirs. The map is the
// whole seam: any pattern under an unmapped root is refused (fails closed),
// so a test can never match against the machine's real /opt/homebrew.
func testRoots(t *testing.T) (map[string]string, rootFunc) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]string{
		"/opt/homebrew":              filepath.Join(base, "opt-homebrew"),
		"/usr/local":                 filepath.Join(base, "usr-local"),
		"/home/linuxbrew/.linuxbrew": filepath.Join(base, "linuxbrew"),
	}
	return roots, func(pattern string) (string, bool) {
		for real, fake := range roots {
			if pattern == real || strings.HasPrefix(pattern, real+"/") {
				return fake + strings.TrimPrefix(pattern, real), true
			}
		}
		return "", false
	}
}

// place creates an empty file at path (and its parents) and returns path.
func place(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// linkAs makes a bin-name symlink to target in its own dir and returns it.
func linkAs(t *testing.T, name, target string) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), name)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// installLayout is one documented real target: under an absolute root (root
// set) or under the fake home (root empty).
type installLayout struct {
	name   string
	root   string // "/opt/homebrew", …, or "" for a "~/" layout
	rel    string // path below the root or the home
	cli    string // the expected identity
	script bool   // run by an interpreter, else the executable itself
	interp string // interpreter basename for a script
}

var documentedLayouts = []installLayout{
	{"claude native installer", "", ".local/share/claude/versions/2.1.300", "claude", false, ""},
	{"claude Homebrew cask claude-code", "/opt/homebrew", "Caskroom/claude-code/2.1.292/claude", "claude", false, ""},
	{"claude Homebrew cask claude-code@latest", "/usr/local", "Caskroom/claude-code@latest/2.1.303/claude", "claude", false, ""},
	{"claude npm, default prefix", "/usr/local", "lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe", "claude", false, ""},
	{"claude npm, Linux Homebrew node", "/home/linuxbrew/.linuxbrew", "lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe", "claude", false, ""},
	{"claude npm, nvm", "", ".nvm/versions/node/v24.1.0/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe", "claude", false, ""},
	{"codex Homebrew cask", "/opt/homebrew", "Caskroom/codex/0.163.0/bin/codex", "codex", false, ""},
	{"codex npm, Homebrew node", "/opt/homebrew", "lib/node_modules/@openai/codex/bin/codex.js", "codex", true, "node"},
	{"gemini npm, default prefix", "/usr/local", "lib/node_modules/@google/gemini-cli/bundle/gemini.js", "gemini", true, "node"},
	{"gemini Homebrew formula", "/opt/homebrew", "Cellar/gemini-cli/0.64.0/libexec/lib/node_modules/@google/gemini-cli/bundle/gemini.js", "gemini", true, "node"},
	{"openclaw npm, nvm", "", ".nvm/versions/node/v24.1.0/lib/node_modules/openclaw/openclaw.mjs", "openclaw", true, "node"},
	{"aider uv tool", "", ".local/share/uv/tools/aider-chat/bin/aider", "aider", true, "python3.12"},
	{"aider pipx, Linux", "", ".local/share/pipx/venvs/aider-chat/bin/aider", "aider", true, "python3.12"},
	{"aider pipx, macOS", "", "Library/Application Support/pipx/venvs/aider-chat/bin/aider", "aider", true, "python3.12"},
	{"aider pipx, legacy home", "", ".local/pipx/venvs/aider-chat/bin/aider", "aider", true, "python3.12"},
}

// lay creates the layout's real target and returns it.
func (l installLayout) lay(t *testing.T, home string, roots map[string]string) string {
	t.Helper()
	if l.root == "" {
		return place(t, filepath.Join(home, l.rel))
	}
	fake, ok := roots[l.root]
	if !ok {
		t.Fatalf("%s: root %s is not mapped", l.name, l.root)
	}
	return place(t, filepath.Join(fake, l.rel))
}

// identifyThroughLink runs identifyCLI on the layout reached through a bin
// symlink named like the CLI: as the executable itself, or as the script an
// interpreter runs.
func identifyThroughLink(t *testing.T, l installLayout, target string, root rootFunc) (string, bool) {
	t.Helper()
	link := linkAs(t, l.cli, target)
	if !l.script {
		return identifyCLI(link, []string{l.cli}, root)
	}
	interp := place(t, filepath.Join(t.TempDir(), "bin", l.interp))
	return identifyCLI(interp, []string{interp, link}, root)
}

func TestIdentifyCLI_StandardInstallsAreIdentifiedThroughASymlink(t *testing.T) {
	for _, l := range documentedLayouts {
		t.Run(l.name, func(t *testing.T) {
			home := identityHome(t, nil)
			roots, root := testRoots(t)
			target := l.lay(t, home, roots)
			got, ok := identifyThroughLink(t, l, target, root)
			if !ok || got != l.cli {
				t.Fatalf("identified as %q, %v; want %q, true", got, ok, l.cli)
			}
		})
	}
}

// A copy beside, but outside, the standard locations is not identified.
func TestIdentifyCLI_CopyBesideAStandardInstallIsNotIdentified(t *testing.T) {
	beside := []installLayout{
		{"another cask's binary", "/opt/homebrew", "Caskroom/ryshother/1.0/claude", "claude", false, ""},
		{"one level deeper in the cask", "/opt/homebrew", "Caskroom/claude-code/2.1.292/extra/claude", "claude", false, ""},
		{"a look-alike npm package", "/usr/local", "lib/node_modules/@anthropic-ai/claude-code-fork/bin/claude.exe", "claude", false, ""},
		{"the npm bin dir itself", "/usr/local", "bin/claude.exe", "claude", false, ""},
		{"a look-alike script in the package", "/opt/homebrew", "lib/node_modules/@openai/codex/bin/other.js", "codex", true, "node"},
		{"the formula outside libexec", "/opt/homebrew", "Cellar/gemini-cli/0.64.0/gemini.js", "gemini", true, "node"},
		{"pipx venv, other script", "", ".local/share/pipx/venvs/aider-chat/bin/ryshother", "aider", true, "python3.12"},
		{"uv tools dir, no env", "", ".local/share/uv/tools/aider", "aider", true, "python3.12"},
	}
	for _, l := range beside {
		t.Run(l.name, func(t *testing.T) {
			home := identityHome(t, nil)
			roots, root := testRoots(t)
			target := l.lay(t, home, roots)
			if got, ok := identifyThroughLink(t, l, target, root); ok {
				t.Fatalf("identified as %q; want not identified", got)
			}
		})
	}
}

// The seam fails closed: with no root mapped, an absolute standard layout is
// not identified, and nothing falls back to the real root.
func TestIdentifyCLI_UnmappedRootIsNotIdentified(t *testing.T) {
	home := identityHome(t, nil)
	roots, _ := testRoots(t)
	refuse := func(string) (string, bool) { return "", false }
	for _, l := range documentedLayouts {
		if l.root == "" {
			continue
		}
		target := l.lay(t, home, roots)
		if got, ok := identifyThroughLink(t, l, target, refuse); ok {
			t.Errorf("%s: identified as %q with no root mapped; want not identified", l.name, got)
		}
	}
}

// (B), rr-msg-333: plausible override variables, plus one name made up at
// run time, change nothing. IdentifyCLI reads only HOME (for "~/"
// locations); a decoy laid out where these variables point is not identified,
// and a standard install is identified exactly as without them.
func TestIdentifyCLI_IgnoresInstallRootEnvironment(t *testing.T) {
	identityHome(t, nil)
	decoyRoot := t.TempDir()
	decoy := place(t, filepath.Join(decoyRoot, "Caskroom", "claude-code", "2.1.292", "claude"))
	script := place(t, filepath.Join(decoyRoot, "lib", "node_modules", "@openai", "codex", "bin", "codex.js"))
	node := place(t, filepath.Join(t.TempDir(), "node"))
	for _, name := range []string{
		"RYSH_INSTALL_ROOT", "RYSH_PROXY_INSTALL_ROOTS", "RYSH_KNOWN_CLI_DIRS",
		"HOMEBREW_PREFIX", "HOMEBREW_CASKROOM", "HOMEBREW_CELLAR",
		"NPM_CONFIG_PREFIX", "npm_config_prefix", "PREFIX",
		"PIPX_HOME", "UV_TOOL_DIR", "XDG_DATA_HOME", "NVM_DIR",
		randomEnvName(t),
	} {
		t.Setenv(name, decoyRoot)
	}
	if got, ok := IdentifyCLI(decoy, []string{"claude"}); ok {
		t.Errorf("a decoy cask under an env-var root was identified as %q", got)
	}
	if got, ok := IdentifyCLI(node, []string{node, script}); ok {
		t.Errorf("a decoy npm script under an env-var root was identified as %q", got)
	}
	home, _ := os.UserHomeDir()
	native := place(t, filepath.Join(home, ".local", "share", "claude", "versions", "2.1.300"))
	if got, ok := IdentifyCLI(native, nil); !ok || got != "claude" {
		t.Errorf("with the variables set, the native install is identified as %q, %v; want \"claude\", true", got, ok)
	}
}

// randomEnvName makes up an environment variable name at run time, so no
// override name could be guessed from the test's source.
func randomEnvName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "RYSH_" + strings.ToUpper(hex.EncodeToString(b))
}

// (A), rr-msg-333: a release build has no reachable override of the install
// locations. It checks the non-test files on the identity path: this package,
// plus its callers in internal/actors.
//   - identifyCLI is called once, by IdentifyCLI, with installedRoot.
//   - installedRoot, profiles, brewPrefixes, npmPrefixes and claudeNpmPlatforms
//     are never assigned
//     after their declaration, and no package variable holds a rootFunc. So
//     the root map can be supplied only by a _test.go caller of identifyCLI.
//   - Exactly ONE env-derived read is allowed (rr-msg-335 (i)):
//     os.UserHomeDir (HOME, which is what "~/" means), called once, in
//     expandHome in compat.go. Any other call site of Getenv, LookupEnv,
//     Environ, ExpandEnv, Expand or UserHomeDir fails.
func TestInstallLocationsHaveNoOverrideInARelease(t *testing.T) {
	fset := token.NewFileSet()
	var files []*ast.File
	var names []string
	add := func(path string) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		names = append(names, path)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			add(n)
		}
	}
	for _, n := range []string{
		"pane_proxy_compat.go", "pane_program.go",
		"pane_procname_darwin.go", "pane_procname_linux.go", "pane_procname_other.go",
	} {
		add(filepath.Join("..", "actors", n))
	}

	guarded := map[string]bool{"installedRoot": true, "profiles": true, "brewPrefixes": true, "npmPrefixes": true, "claudeNpmPlatforms": true}
	envFuncs := map[string]bool{"Getenv": true, "LookupEnv": true, "Environ": true, "ExpandEnv": true, "Expand": true}
	var calls, badCalls, assigns, rootVars, envReads, homeReads int
	var homeSites []string
	for i, file := range files {
		// internal/actors has an unrelated identifyCLI(pid, comm) of its own;
		// proxy's unexported identifyCLI can only be called from this package.
		inProxy := !strings.HasPrefix(names[i], "..")
		funcAt := enclosingFuncs(file)
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && inProxy && id.Name == "identifyCLI" {
					calls++
					if len(x.Args) != 3 || !isIdent(x.Args[2], "installedRoot") {
						badCalls++
						t.Errorf("%s: identifyCLI called with a root other than installedRoot", fset.Position(x.Pos()))
					}
				}
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (isIdent(sel.X, "os") || isIdent(sel.X, "syscall")) {
					switch {
					case envFuncs[sel.Sel.Name]:
						envReads++
						t.Errorf("%s: the identity path reads the environment (%s)", fset.Position(x.Pos()), sel.Sel.Name)
					case sel.Sel.Name == "UserHomeDir":
						homeReads++
						homeSites = append(homeSites, names[i]+":"+funcAt(x.Pos()))
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if root := rootIdent(lhs); guarded[root] {
						assigns++
						t.Errorf("%s: %s is assigned outside its declaration", fset.Position(x.Pos()), root)
					}
				}
			case *ast.ValueSpec:
				if isIdent(x.Type, "rootFunc") {
					rootVars++
					t.Errorf("%s: a variable holds a rootFunc", fset.Position(x.Pos()))
				}
			}
			return true
		})
	}
	if calls != 1 {
		t.Errorf("identifyCLI is called %d times in non-test code, want exactly 1 (IdentifyCLI)", calls)
	}
	if homeReads != 1 || len(homeSites) != 1 || homeSites[0] != "compat.go:expandHome" {
		t.Errorf("os.UserHomeDir call sites on the identity path = %v, want exactly [compat.go:expandHome]", homeSites)
	}
	if envReads != 0 {
		t.Errorf("%d other env-derived reads on the identity path, want 0", envReads)
	}
	t.Logf("files=%d identifyCLI calls=%d (bad=%d) guarded assignments=%d rootFunc vars=%d env reads=%d UserHomeDir=%d",
		len(names), calls, badCalls, assigns, rootVars, envReads, homeReads)
}

// enclosingFuncs returns a lookup from a position in file to the name of the
// top-level function that contains it ("" outside any function).
func enclosingFuncs(file *ast.File) func(token.Pos) string {
	return func(pos token.Pos) string {
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Pos() <= pos && pos < fd.End() {
				return fd.Name.Name
			}
		}
		return ""
	}
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// rootIdent returns the base identifier of an assignment target such as
// profiles["x"] or npmPrefixes[0].
func rootIdent(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.IndexExpr:
			e = x.X
		case *ast.SelectorExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		default:
			return ""
		}
	}
}

// npm claude (rr-msg-337): install.cjs hard-links the platform package's
// binary to bin/claude.exe. For each of npm's two global layouts (nested and
// hoisted), BOTH names of the one inode are identified.
func TestIdentifyCLI_NpmClaudeHardLinkBothNamesAreIdentified(t *testing.T) {
	cases := []struct{ name, root, prefix, platform, layout string }{
		{"nested, default prefix, darwin-arm64", "/usr/local", "", "darwin-arm64", "nested"},
		{"hoisted, default prefix, darwin-arm64", "/usr/local", "", "darwin-arm64", "hoisted"},
		{"nested, Homebrew node, linux-x64-musl", "/opt/homebrew", "", "linux-x64-musl", "nested"},
		{"hoisted, Linux Homebrew node, linux-arm64", "/home/linuxbrew/.linuxbrew", "", "linux-arm64", "hoisted"},
		{"nested, nvm, darwin-x64", "", ".nvm/versions/node/v24.1.0", "darwin-x64", "nested"},
		{"hoisted, nvm, linux-arm64-musl", "", ".nvm/versions/node/v24.1.0", "linux-arm64-musl", "hoisted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := identityHome(t, nil)
			roots, root := testRoots(t)
			prefix := filepath.Join(home, c.prefix)
			if c.root != "" {
				prefix = roots[c.root]
			}
			mods := filepath.Join(prefix, "lib", "node_modules")
			pkg := "claude-code-" + c.platform
			src := filepath.Join(mods, "@anthropic-ai", pkg, "claude")
			if c.layout == "nested" {
				src = filepath.Join(mods, "@anthropic-ai", "claude-code", "node_modules", "@anthropic-ai", pkg, "claude")
			}
			dest := filepath.Join(mods, "@anthropic-ai", "claude-code", "bin", "claude.exe")
			place(t, src)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(src, dest); err != nil {
				t.Fatalf("hard link: %v", err)
			}
			for _, name := range []string{dest, src} {
				if got, ok := identifyCLI(name, []string{"claude"}, root); !ok || got != "claude" {
					t.Errorf("%s: identified as %q, %v; want \"claude\", true", filepath.Base(filepath.Dir(name)), got, ok)
				}
			}
		})
	}
}

// Beside the npm claude layouts, and outside them: not identified.
func TestIdentifyCLI_NpmClaudeLookAlikesAreNotIdentified(t *testing.T) {
	cases := []struct{ name, root, rel string }{
		{"a copy one level deeper in the platform package", "/usr/local",
			"lib/node_modules/@anthropic-ai/claude-code-darwin-arm64/bin/claude"},
		{"a look-alike platform package", "/usr/local",
			"lib/node_modules/@anthropic-ai/claude-code-darwin-arm64-fork/claude"},
		{"a platform name not in the table (android)", "/usr/local",
			"lib/node_modules/@anthropic-ai/claude-code-linux-arm64-android/claude"},
		{"a platform name not in the table (win32)", "/opt/homebrew",
			"lib/node_modules/@anthropic-ai/claude-code/node_modules/@anthropic-ai/claude-code-win32-x64/claude"},
		{"a platform name not in the table (freebsd)", "/opt/homebrew",
			"lib/node_modules/@anthropic-ai/claude-code-freebsd-x64/claude"},
		{"Node's $HOME/.node_modules", "", ".node_modules/@anthropic-ai/claude-code-darwin-arm64/claude"},
		{"Node's $HOME/.node_libraries", "", ".node_libraries/@anthropic-ai/claude-code-darwin-arm64/claude"},
		{"Node's {prefix}/lib/node", "/usr/local", "lib/node/@anthropic-ai/claude-code-darwin-arm64/claude"},
		{"a node_modules outside any prefix", "", "projects/node_modules/@anthropic-ai/claude-code-darwin-arm64/claude"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := identityHome(t, nil)
			roots, root := testRoots(t)
			base := home
			if c.root != "" {
				base = roots[c.root]
			}
			path := place(t, filepath.Join(base, c.rel))
			if got, ok := identifyCLI(path, []string{"claude"}, root); ok {
				t.Fatalf("identified as %q; want not identified", got)
			}
		})
	}
}
