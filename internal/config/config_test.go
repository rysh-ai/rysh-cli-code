// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// tempDir is t.TempDir() with symlinks resolved, and it is what every fixture
// here must use to build an EXPECTED path. On macOS t.TempDir() returns a path
// under /var, which is itself a symlink to /private/var; getwd() resolves the
// cwd on purpose (one directory, one identity — see its doc comment), so a
// fixture holding the raw /var spelling fails against a correct result. The
// mismatch is in the fixture, not the resolution.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return resolved
}

// writeConfig writes content to rysh.config.yaml in a fresh temp dir and chdirs
// into it so that findConfigFile() picks it up.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := tempDir(t)
	path := filepath.Join(dir, "rysh.config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Chdir(dir)
	return dir
}

// TestLoadFromExplicitPath verifies that loadFrom honors an explicit config
// path that is NOT one of the default search locations (it is not named
// rysh.config.yaml and lives in a directory we never chdir into).
func TestLoadFromExplicitPath(t *testing.T) {
	// Chdir into an empty dir so findConfigFile() would find nothing.
	t.Chdir(tempDir(t))

	dir := tempDir(t)
	path := filepath.Join(dir, "custom.config")
	content := "ui:\n  initial_tabs: 7\nrysh:\n  session_name: \"explicit-cfg\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFrom(path, new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom returned error: %v", err)
	}
	if cfg.InitialTabs != 7 {
		t.Errorf("InitialTabs = %d, want 7 (from explicit --config path)", cfg.InitialTabs)
	}
	if cfg.SessionName != "explicit-cfg" {
		t.Errorf("SessionName = %q, want %q (from explicit --config path)", cfg.SessionName, "explicit-cfg")
	}
}

// TestLoadFromEmptyPathFallsBackToSearch verifies that an empty explicit path
// makes loadFrom behave like load (searching ./rysh.config.yaml).
func TestLoadFromEmptyPathFallsBackToSearch(t *testing.T) {
	writeConfig(t, "rysh:\n  session_name: \"searched\"\n")
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom returned error: %v", err)
	}
	if cfg.SessionName != "searched" {
		t.Errorf("SessionName = %q, want %q (from default search)", cfg.SessionName, "searched")
	}
}

// TestLoadFromFileMalformedYAMLReturnsError verifies that a value of the wrong
// type (a string where an int is expected) surfaces a parse error rather than
// being silently swallowed — and that on failure the defaults are preserved
// rather than partially applied.
func TestLoadFromFileMalformedYAMLReturnsError(t *testing.T) {
	dir := tempDir(t)
	path := filepath.Join(dir, "rysh.config.yaml")
	content := "ui:\n  initial_tabs: not-a-number\nsession:\n  name: \"pera-dev\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadFromFile(path, applyDefaults())
	if err == nil {
		t.Fatalf("expected parse error for non-numeric initial_tabs, got nil")
	}
	// On parse failure the defaults are preserved, not partially applied.
	if got.SessionName != "default" {
		t.Errorf("SessionName = %q, want default on parse failure", got.SessionName)
	}
	if got.WorkingDirectory != "" {
		t.Errorf("WorkingDirectory = %q, want empty on parse failure", got.WorkingDirectory)
	}
}

// TestLoadWarnsOnParseError verifies that load() writes a visible warning when
// the config file cannot be parsed instead of silently using defaults.
func TestLoadWarnsOnParseError(t *testing.T) {
	writeConfig(t, "ui:\n  initial_tabs: not-a-number\n")

	var buf strings.Builder
	cfg, err := load(&buf)
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	out := buf.String()
	if !strings.Contains(out, "failed to parse config file") {
		t.Errorf("warning not emitted; stderr = %q", out)
	}
	// Defaults still apply so the daemon can start.
	if cfg.SessionName != "default" {
		t.Errorf("SessionName = %q, want default", cfg.SessionName)
	}
}

// TestLoadWorkingDirectoryExpandsTilde verifies the happy path: a
// working_directory with a leading tilde is honored and expanded. (Unlike TOML,
// YAML does not require the value to be quoted, but quoting still works.)
func TestLoadWorkingDirectoryExpandsTilde(t *testing.T) {
	// Pin HOME and create the target directory. load() deliberately blanks a
	// working_directory that does not exist (see resolveWorkingDirectory +
	// isExistingDir, so a typo'd path cannot strand panes), which means this
	// test only exercises tilde expansion if the expanded path really exists.
	// Without this it passed solely on machines that happened to have
	// ~/root/github/rysh-ai — green locally for the author, red everywhere else.
	home := tempDir(t)
	t.Setenv("HOME", home)
	want := filepath.Join(home, "root", "github", "rysh-ai")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	writeConfig(t, "rysh:\n  working_directory: \"~/root/github/rysh-ai\"\n  session_name: \"pera-dev\"\n")

	var buf strings.Builder
	cfg, err := load(&buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WorkingDirectory != want {
		t.Errorf("WorkingDirectory = %q, want %q", cfg.WorkingDirectory, want)
	}
	if cfg.SessionName != "pera-dev" {
		t.Errorf("SessionName = %q, want pera-dev", cfg.SessionName)
	}
	if w := buf.String(); w != "" {
		t.Errorf("unexpected warning: %q", w)
	}
}

// TestResolvedWorkspacesFallback verifies that a config with no workspace
// entries synthesizes a single workspace from the top-level settings, so
// existing single-workspace configs keep working unchanged.
func TestResolvedWorkspacesFallback(t *testing.T) {
	cfg := applyDefaults()
	cfg.SessionName = "solo"
	cfg.InitialTabs = 2
	cfg.InitialPanes = 3
	cfg.WorkingDirectory = "/tmp/solo"

	ws := cfg.ResolvedWorkspaces()
	if len(ws) != 1 {
		t.Fatalf("len = %d, want 1", len(ws))
	}
	got := ws[0]
	if got.Name != "solo" || got.InitialTabs != 2 || got.InitialPanes != 3 || got.WorkingDirectory != "/tmp/solo" {
		t.Errorf("fallback workspace = %+v", got)
	}
}

// TestResolveUpstreamInherits verifies a workspace with no upstream override
// inherits the session block, with the namespace defaulting to its name.
func TestResolveUpstreamInherits(t *testing.T) {
	session := UpstreamConfig{Enabled: true, URL: "https://rysh.ai", APIKey: "rysh_session", DefaultShareMode: "control"}
	wc := WorkspaceConfig{Name: "main"}
	up := wc.ResolveUpstream(session)
	if !up.Enabled || up.URL != "https://rysh.ai" || up.APIKey != "rysh_session" {
		t.Errorf("inherited upstream = %+v", up)
	}
	if up.Workspace != "main" {
		t.Errorf("namespace = %q, want %q (defaults to workspace name)", up.Workspace, "main")
	}
}

// TestResolveUpstreamOverrideInheritsBlanks verifies a per-workspace upstream
// override is used, with empty connection fields inherited from the session.
func TestResolveUpstreamOverrideInheritsBlanks(t *testing.T) {
	session := UpstreamConfig{Enabled: true, URL: "https://rysh.ai", APIKey: "rysh_session", ReconnectInterval: "5s"}
	wc := WorkspaceConfig{
		Name:     "dev-macmini",
		Upstream: &UpstreamConfig{Enabled: true, APIKey: "rysh_workspace"},
	}
	up := wc.ResolveUpstream(session)
	if up.APIKey != "rysh_workspace" {
		t.Errorf("APIKey = %q, want workspace override", up.APIKey)
	}
	if up.URL != "https://rysh.ai" {
		t.Errorf("URL = %q, want inherited from session", up.URL)
	}
	if up.ReconnectInterval != "5s" {
		t.Errorf("ReconnectInterval = %q, want inherited", up.ReconnectInterval)
	}
	if up.Workspace != "dev-macmini" {
		t.Errorf("namespace = %q, want %q", up.Workspace, "dev-macmini")
	}
}

// TestResolveUpstreamFromYAML verifies the nested per-workspace upstream mapping
// parses from a workspace list entry.
func TestResolveUpstreamFromYAML(t *testing.T) {
	writeConfig(t, `
upstream:
  enabled: true
  url: "https://rysh.ai"
  api_key: "rysh_session"

workspace:
  - name: "dev-macmini"
    upstream:
      enabled: true
      api_key: "rysh_workspaceKey"
`)
	cfg, err := load(new(strings.Builder))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ws := cfg.ResolvedWorkspaces()
	if len(ws) != 1 || ws[0].Upstream == nil {
		t.Fatalf("workspace upstream not parsed: %+v", ws)
	}
	up := ws[0].ResolveUpstream(cfg.Upstream)
	if up.APIKey != "rysh_workspaceKey" || up.URL != "https://rysh.ai" || up.Workspace != "dev-macmini" {
		t.Errorf("resolved upstream = %+v", up)
	}
}

// TestResolvedWorkspacesFromConfig verifies parsing of the workspace list and
// that unset per-workspace fields inherit the top-level defaults.
func TestResolvedWorkspacesFromConfig(t *testing.T) {
	writeConfig(t, `
ui:
  initial_tabs: 1
  initial_panes: 1

workspace:
  - name: "main"
    initial_panes: 2
  - name: "infra"
    initial_tabs: 3
`)
	cfg, err := load(new(strings.Builder))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ws := cfg.ResolvedWorkspaces()
	if len(ws) != 2 {
		t.Fatalf("len = %d, want 2", len(ws))
	}
	if ws[0].Name != "main" || ws[0].InitialPanes != 2 || ws[0].InitialTabs != 1 {
		t.Errorf("ws[0] = %+v (want name=main panes=2 tabs=1)", ws[0])
	}
	if ws[1].Name != "infra" || ws[1].InitialTabs != 3 || ws[1].InitialPanes != 1 {
		t.Errorf("ws[1] = %+v (want name=infra tabs=3 panes=1)", ws[1])
	}
}

func TestResolveWorkingDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	root := tempDir(t) // e.g. /tmp/xxx (acts as a project root)
	plainCfg := filepath.Join(root, "rysh.config.yaml")
	dotRyshDir := filepath.Join(root, ".rysh")
	dotRyshCfg := filepath.Join(dotRyshDir, "rysh.config.yaml")

	tests := []struct {
		name       string
		configFile string
		explicit   string
		want       string
	}{
		{
			name:       "explicit absolute path wins",
			configFile: plainCfg,
			explicit:   "/var/data/work",
			want:       "/var/data/work",
		},
		{
			name:       "explicit relative path resolves against config dir",
			configFile: plainCfg,
			explicit:   "sub/dir",
			want:       filepath.Join(root, "sub", "dir"),
		},
		{
			name:       "explicit tilde expands to home",
			configFile: plainCfg,
			explicit:   "~/projects/app",
			want:       filepath.Join(home, "projects", "app"),
		},
		{
			name:       "no explicit, plain dir uses config directory",
			configFile: plainCfg,
			explicit:   "",
			want:       root,
		},
		{
			name:       "no explicit, .rysh dir uses parent of .rysh",
			configFile: dotRyshCfg,
			explicit:   "",
			want:       root,
		},
		{
			name:       "no config file and no explicit yields empty",
			configFile: "",
			explicit:   "",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveWorkingDirectory(tt.configFile, tt.explicit)
			if got != tt.want {
				t.Errorf("resolveWorkingDirectory(%q, %q) = %q, want %q",
					tt.configFile, tt.explicit, got, tt.want)
			}
		})
	}
}

func TestResolveWorkingDirectoryExplicitWithoutConfigFile(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	got := resolveWorkingDirectory("", "relative/path")
	want := filepath.Join(cwd, "relative", "path")
	if got != want {
		t.Errorf("resolveWorkingDirectory(\"\", \"relative/path\") = %q, want %q", got, want)
	}
}

// isolateConfigEnv puts the test in a fresh working directory with a fresh,
// empty $HOME so resolveConfig() only sees files this test creates, and
// neutralizes the env vars that could override the resolved rysh dir or the
// storage paths derived from it (RYSH_DIR, RYSH_SESSION_DIR, RYSH_NATS_DATA_DIR,
// XDG_STATE_HOME) so the host environment cannot perturb the test. rysh state is
// always project-local, so with no config file the rysh dir is deterministically
// <cwd>/.rysh (see defaultRyshDir). It returns the resolved working dir
// (os.Getwd, which may differ from the t.TempDir string on platforms that
// symlink the temp root) and the home dir.
func isolateConfigEnv(t *testing.T) (cwd, home string) {
	t.Helper()
	cwdTmp := tempDir(t)
	homeTmp := tempDir(t)
	t.Setenv("HOME", homeTmp)
	t.Setenv("RYSH_DIR", "")
	t.Setenv("RYSH_SESSION_DIR", "")
	t.Setenv("RYSH_NATS_DATA_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Chdir(cwdTmp)
	got, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return got, homeTmp
}

// localRoot is the expected project-local rysh state root for a no-config test:
// <cwd>/.rysh. rysh state is always relative to the working directory; there is
// no global fallback.
func localRoot(cwd string) string {
	return filepath.Join(cwd, ".rysh")
}

// touchConfig writes a minimal valid rysh.config.yaml at path, creating parents.
func touchConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("session:\n  name: \"x\"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestResolveConfig exercises the documented search order and the rysh dir each
// location implies, including precedence between competing locations.
func TestResolveConfig(t *testing.T) {
	t.Run("cwd plain wins, rysh dir is sibling .rysh", func(t *testing.T) {
		cwd, home := isolateConfigEnv(t)
		touchConfig(t, filepath.Join(cwd, "rysh.config.yaml"))
		// Lower-precedence locations must be ignored.
		touchConfig(t, filepath.Join(cwd, ".rysh", "rysh.config.yaml"))
		touchConfig(t, filepath.Join(home, ".rysh", "rysh.config.yaml"))

		file, ryshDir := resolveConfig()
		if want := filepath.Join(cwd, "rysh.config.yaml"); file != want {
			t.Errorf("configFile = %q, want %q", file, want)
		}
		if want := filepath.Join(cwd, ".rysh"); ryshDir != want {
			t.Errorf("ryshDir = %q, want %q", ryshDir, want)
		}
	})

	t.Run("cwd/.rysh, rysh dir is that .rysh dir", func(t *testing.T) {
		cwd, home := isolateConfigEnv(t)
		touchConfig(t, filepath.Join(cwd, ".rysh", "rysh.config.yaml"))
		touchConfig(t, filepath.Join(home, ".rysh", "rysh.config.yaml")) // lower precedence

		file, ryshDir := resolveConfig()
		if want := filepath.Join(cwd, ".rysh", "rysh.config.yaml"); file != want {
			t.Errorf("configFile = %q, want %q", file, want)
		}
		if want := filepath.Join(cwd, ".rysh"); ryshDir != want {
			t.Errorf("ryshDir = %q, want %q", ryshDir, want)
		}
	})

	t.Run("home configs are never searched (no global locations)", func(t *testing.T) {
		_, home := isolateConfigEnv(t)
		// Configs only in the home directory must be ignored: rysh state is
		// project-local, with no ~/.rysh or ~/.config/rysh search.
		touchConfig(t, filepath.Join(home, ".rysh", "rysh.config.yaml"))
		touchConfig(t, filepath.Join(home, ".config", "rysh", "rysh.config.yaml"))

		file, ryshDir := resolveConfig()
		if file != "" || ryshDir != "" {
			t.Errorf("resolveConfig() = (%q, %q), want both empty (home is never searched)", file, ryshDir)
		}
	})

	t.Run("none found yields empties; defaultRyshDir is <cwd>/.rysh", func(t *testing.T) {
		cwd, _ := isolateConfigEnv(t)
		file, ryshDir := resolveConfig()
		if file != "" || ryshDir != "" {
			t.Errorf("resolveConfig() = (%q, %q), want both empty", file, ryshDir)
		}
		if want := localRoot(cwd); defaultRyshDir() != want {
			t.Errorf("defaultRyshDir() = %q, want %q (project-local)", defaultRyshDir(), want)
		}
	})
}

// TestResolveConfigCanonicalizesSymlinkedCwd pins the aliasing fix: a project
// entered THROUGH a symlink must resolve to the same rysh dir as the target,
// never a second one named after the link.
//
// The setup is not contrived — it is what a shell hands a process after "cd"
// through a symlink. The shell exports PWD as the route taken, and os.Getwd
// returns PWD verbatim whenever it stats to the same directory as ".", so the
// link's spelling reaches every cwd-derived path: the rysh dir, the session
// registry inside it, and the config file. That is how one directory reached
// as ~/root/github/rysh-ai (a symlink) and as ~/root/github/agentic-zellij
// (its target) registered two sessions over ONE physical .rysh, each claiming
// the same pinned nats.port and neither visible to the other's name check —
// leaving "rysh create" refusing a name it could not account for.
func TestResolveConfigCanonicalizesSymlinkedCwd(t *testing.T) {
	target, _ := isolateConfigEnv(t)
	touchConfig(t, filepath.Join(target, "rysh.config.yaml"))

	link := filepath.Join(tempDir(t), "alias")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	t.Chdir(link)
	// Set PWD after the chdir: t.Chdir already points it at the link, but
	// stating it here is the whole premise of the test rather than a detail
	// inherited from the helper.
	t.Setenv("PWD", link)

	file, ryshDir := resolveConfig()
	if want := filepath.Join(target, "rysh.config.yaml"); file != want {
		t.Errorf("resolveConfig() file = %q, want %q (the route in must not become the identity)", file, want)
	}
	if want := localRoot(target); ryshDir != want {
		t.Errorf("resolveConfig() ryshDir = %q, want %q", ryshDir, want)
	}
	if want := localRoot(target); defaultRyshDir() != want {
		t.Errorf("defaultRyshDir() = %q, want %q", defaultRyshDir(), want)
	}
}

// TestRyshDirForConfig covers the explicit "--config <path>" mapping.
func TestRyshDirForConfig(t *testing.T) {
	tmp := tempDir(t)

	plain := filepath.Join(tmp, "rysh.config.yaml")
	if got, want := ryshDirForConfig(plain), filepath.Join(tmp, ".rysh"); got != want {
		t.Errorf("ryshDirForConfig(plain) = %q, want %q", got, want)
	}

	inside := filepath.Join(tmp, ".rysh", "rysh.config.yaml")
	if got, want := ryshDirForConfig(inside), filepath.Join(tmp, ".rysh"); got != want {
		t.Errorf("ryshDirForConfig(inside .rysh) = %q, want %q", got, want)
	}

	if got := ryshDirForConfig(""); got != "" {
		t.Errorf("ryshDirForConfig(\"\") = %q, want empty", got)
	}
}

// TestLoadPopulatesConfigFileAndRyshDir verifies the resolved decision is
// recorded on Config so the session can be made aware of it.
func TestLoadPopulatesConfigFileAndRyshDir(t *testing.T) {
	cwd, _ := isolateConfigEnv(t)
	touchConfig(t, filepath.Join(cwd, "rysh.config.yaml"))

	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if want := filepath.Join(cwd, "rysh.config.yaml"); cfg.ConfigFile != want {
		t.Errorf("cfg.ConfigFile = %q, want %q", cfg.ConfigFile, want)
	}
	if want := filepath.Join(cwd, ".rysh"); cfg.RyshDir != want {
		t.Errorf("cfg.RyshDir = %q, want %q", cfg.RyshDir, want)
	}
}

// TestLoadRyshDirDefaultWhenNoConfig verifies RyshDir is always populated, and
// that with no config file it falls back to the project-local <cwd>/.rysh.
func TestLoadRyshDirDefaultWhenNoConfig(t *testing.T) {
	cwd, _ := isolateConfigEnv(t)
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.ConfigFile != "" {
		t.Errorf("cfg.ConfigFile = %q, want empty (no config file)", cfg.ConfigFile)
	}
	if want := localRoot(cwd); cfg.RyshDir != want {
		t.Errorf("cfg.RyshDir = %q, want %q (project-local)", cfg.RyshDir, want)
	}
}

// TestLoadFromExplicitPathSetsRyshDir verifies an explicit "--config" path also
// resolves a rysh dir (a sibling .rysh of the config file).
func TestLoadFromExplicitPathSetsRyshDir(t *testing.T) {
	t.Setenv("RYSH_DIR", "")
	t.Chdir(tempDir(t))
	dir := tempDir(t)
	path := filepath.Join(dir, "custom.config")
	if err := os.WriteFile(path, []byte("session:\n  name: \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFrom(path, new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.ConfigFile != path {
		t.Errorf("cfg.ConfigFile = %q, want %q", cfg.ConfigFile, path)
	}
	if want := filepath.Join(dir, ".rysh"); cfg.RyshDir != want {
		t.Errorf("cfg.RyshDir = %q, want %q", cfg.RyshDir, want)
	}
}

// TestRyshDirEnvOverride verifies RYSH_DIR overrides the resolved rysh dir.
func TestRyshDirEnvOverride(t *testing.T) {
	cwd, _ := isolateConfigEnv(t)
	touchConfig(t, filepath.Join(cwd, "rysh.config.yaml"))
	override := tempDir(t)
	t.Setenv("RYSH_DIR", override)

	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.RyshDir != override {
		t.Errorf("cfg.RyshDir = %q, want %q (RYSH_DIR override)", cfg.RyshDir, override)
	}
	// Storage paths must follow the RYSH_DIR override, not the resolved rysh dir.
	if want := filepath.Join(override, "sessions"); cfg.SessionDir != want {
		t.Errorf("cfg.SessionDir = %q, want %q (follows RYSH_DIR)", cfg.SessionDir, want)
	}
	if want := filepath.Join(override, "nats"); cfg.NATS.DataDir != want {
		t.Errorf("cfg.NATS.DataDir = %q, want %q (follows RYSH_DIR)", cfg.NATS.DataDir, want)
	}
}

// TestStorageDirsDeriveFromRyshDir verifies that the session registry and NATS
// data dir default to <rysh-dir>/sessions and <rysh-dir>/nats.
func TestStorageDirsDeriveFromRyshDir(t *testing.T) {
	t.Run("cwd config -> <cwd>/.rysh", func(t *testing.T) {
		cwd, _ := isolateConfigEnv(t)
		touchConfig(t, filepath.Join(cwd, "rysh.config.yaml"))

		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		ryshDir := filepath.Join(cwd, ".rysh")
		if want := filepath.Join(ryshDir, "sessions"); cfg.SessionDir != want {
			t.Errorf("cfg.SessionDir = %q, want %q", cfg.SessionDir, want)
		}
		if want := filepath.Join(ryshDir, "nats"); cfg.NATS.DataDir != want {
			t.Errorf("cfg.NATS.DataDir = %q, want %q", cfg.NATS.DataDir, want)
		}
	})

	t.Run("no config -> <cwd>/.rysh", func(t *testing.T) {
		cwd, _ := isolateConfigEnv(t)

		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		root := localRoot(cwd)
		if want := filepath.Join(root, "sessions"); cfg.SessionDir != want {
			t.Errorf("cfg.SessionDir = %q, want %q", cfg.SessionDir, want)
		}
		if want := filepath.Join(root, "nats"); cfg.NATS.DataDir != want {
			t.Errorf("cfg.NATS.DataDir = %q, want %q", cfg.NATS.DataDir, want)
		}
	})

	t.Run("no config + XDG_STATE_HOME is ignored -> <cwd>/.rysh", func(t *testing.T) {
		cwd, _ := isolateConfigEnv(t)
		// XDG_STATE_HOME no longer relocates rysh state — there is no global root.
		t.Setenv("XDG_STATE_HOME", tempDir(t))

		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		root := localRoot(cwd)
		if want := filepath.Join(root, "sessions"); cfg.SessionDir != want {
			t.Errorf("cfg.SessionDir = %q, want %q", cfg.SessionDir, want)
		}
		if want := filepath.Join(root, "nats"); cfg.NATS.DataDir != want {
			t.Errorf("cfg.NATS.DataDir = %q, want %q", cfg.NATS.DataDir, want)
		}
	})
}

// TestStorageDirsExplicitOverridesWin verifies that config-file nats.data_dir
// and the RYSH_SESSION_DIR / RYSH_NATS_DATA_DIR env vars still override the
// rysh-dir-derived defaults. (session.dir was removed: the registry is always
// <RyshDir>/sessions, overridable only via RYSH_SESSION_DIR.)
func TestStorageDirsExplicitOverridesWin(t *testing.T) {
	t.Run("config file nats.data_dir wins; session.dir is ignored", func(t *testing.T) {
		cwd, _ := isolateConfigEnv(t)
		// session.dir is a removed key: it must NOT relocate the registry.
		content := "session:\n  dir: \"/custom/sessions\"\nnats:\n  data_dir: \"/custom/nats\"\n"
		touchConfig(t, filepath.Join(cwd, "rysh.config.yaml"))
		if err := os.WriteFile(filepath.Join(cwd, "rysh.config.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		if cfg.SessionDir == "/custom/sessions" {
			t.Errorf("cfg.SessionDir = %q; session.dir should be ignored (registry stays under RyshDir)", cfg.SessionDir)
		}
		if want := filepath.Join(cfg.RyshDir, "sessions"); cfg.SessionDir != want {
			t.Errorf("cfg.SessionDir = %q, want %q (RyshDir-derived default)", cfg.SessionDir, want)
		}
		if cfg.NATS.DataDir != "/custom/nats" {
			t.Errorf("cfg.NATS.DataDir = %q, want %q (config override)", cfg.NATS.DataDir, "/custom/nats")
		}
	})

	t.Run("RYSH_SESSION_DIR and RYSH_NATS_DATA_DIR win", func(t *testing.T) {
		cwd, _ := isolateConfigEnv(t)
		touchConfig(t, filepath.Join(cwd, "rysh.config.yaml"))
		t.Setenv("RYSH_SESSION_DIR", "/env/sessions")
		t.Setenv("RYSH_NATS_DATA_DIR", "/env/nats")

		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		if cfg.SessionDir != "/env/sessions" {
			t.Errorf("cfg.SessionDir = %q, want %q (env override)", cfg.SessionDir, "/env/sessions")
		}
		if cfg.NATS.DataDir != "/env/nats" {
			t.Errorf("cfg.NATS.DataDir = %q, want %q (env override)", cfg.NATS.DataDir, "/env/nats")
		}
	})
}

// TestWorkspaceWorkingDir verifies working-directory resolution now that it is
// per-workspace with an inherited top-level default:
//   - rysh.working_directory (existing) is the inherited default;
//   - a missing rysh.working_directory falls back to the config-file directory;
//   - a per-workspace working_directory that exists wins for that workspace;
//   - a per-workspace working_directory missing on disk is blanked so it inherits
//     rysh.working_directory.
func TestWorkspaceWorkingDir(t *testing.T) {
	t.Run("rysh.working_directory (exists) is the inherited default", func(t *testing.T) {
		isolateConfigEnv(t)
		dir := writeConfig(t, "rysh:\n  working_directory: \"def\"\n")
		target := filepath.Join(dir, "def")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		if cfg.WorkingDirectory != target {
			t.Errorf("WorkingDirectory = %q, want %q", cfg.WorkingDirectory, target)
		}
	})

	t.Run("missing rysh.working_directory falls back to config dir", func(t *testing.T) {
		isolateConfigEnv(t)
		dir := writeConfig(t, "rysh:\n  working_directory: \"nope_missing\"\n")
		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		if cfg.WorkingDirectory != dir {
			t.Errorf("WorkingDirectory = %q, want %q (config-dir fallback)", cfg.WorkingDirectory, dir)
		}
	})

	t.Run("per-workspace working_directory (exists) wins", func(t *testing.T) {
		isolateConfigEnv(t)
		dir := writeConfig(t, "workspace:\n  - name: \"a\"\n    working_directory: \"wsa\"\n")
		target := filepath.Join(dir, "wsa")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		wss := cfg.ResolvedWorkspaces()
		if wss[0].WorkingDirectory != target {
			t.Errorf("workspace[0].WorkingDirectory = %q, want %q", wss[0].WorkingDirectory, target)
		}
	})

	t.Run("missing per-workspace dir inherits rysh.working_directory", func(t *testing.T) {
		isolateConfigEnv(t)
		dir := writeConfig(t, "rysh:\n  working_directory: \"def\"\nworkspace:\n  - name: \"a\"\n    working_directory: \"gone_missing\"\n")
		def := filepath.Join(dir, "def")
		if err := os.MkdirAll(def, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		wss := cfg.ResolvedWorkspaces()
		if wss[0].WorkingDirectory != def {
			t.Errorf("workspace[0].WorkingDirectory = %q, want %q (inherited rysh.working_directory)", wss[0].WorkingDirectory, def)
		}
	})
}

// TestRyshSessionKeys verifies that session_name and upgrade_on_attach parse
// from the top-level rysh: section (moved there from the removed session:).
func TestRyshSessionKeys(t *testing.T) {
	isolateConfigEnv(t)
	writeConfig(t, "rysh:\n  session_name: \"proj\"\n  upgrade_on_attach: \"auto\"\n")
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SessionName != "proj" {
		t.Errorf("SessionName = %q, want %q", cfg.SessionName, "proj")
	}
	if cfg.UpgradeOnAttach != "auto" {
		t.Errorf("UpgradeOnAttach = %q, want %q", cfg.UpgradeOnAttach, "auto")
	}
}

// TestSetWorkspaceWorkingDir verifies that SetWorkspaceWorkingDir writes the
// active workspace's working_directory back to rysh.config.yaml, preserving
// other entries/comments, and that it targets rysh.working_directory when there
// is no workspace: section.
func TestSetWorkspaceWorkingDir(t *testing.T) {
	t.Run("updates the indexed workspace entry, preserves siblings + comments", func(t *testing.T) {
		isolateConfigEnv(t)
		dir := writeConfig(t, "# my config\nworkspace:\n  - name: \"a\"\n    working_directory: \"old\"\n  - name: \"b\"\n")
		path := filepath.Join(dir, "rysh.config.yaml")
		target := filepath.Join(dir, "proj")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}

		if err := SetWorkspaceWorkingDir(path, 0, "a", target); err != nil {
			t.Fatalf("SetWorkspaceWorkingDir: %v", err)
		}

		got := func() string { raw, _ := os.ReadFile(path); return string(raw) }()
		if !strings.Contains(got, target) {
			t.Errorf("config missing new working_directory %q:\n%s", target, got)
		}
		if !strings.Contains(got, "# my config") {
			t.Errorf("config lost its comment:\n%s", got)
		}

		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		wss := cfg.ResolvedWorkspaces()
		if len(wss) != 2 {
			t.Fatalf("workspaces = %d, want 2 (sibling entry preserved)", len(wss))
		}
		if wss[0].WorkingDirectory != target {
			t.Errorf("workspace[0].WorkingDirectory = %q, want %q", wss[0].WorkingDirectory, target)
		}
		// Entry "b" was untouched: empty -> inherits config dir, not target.
		if wss[1].WorkingDirectory == target {
			t.Errorf("workspace[1] should not have changed; got %q", wss[1].WorkingDirectory)
		}
	})

	t.Run("no workspace section -> sets rysh.working_directory", func(t *testing.T) {
		isolateConfigEnv(t)
		dir := writeConfig(t, "rysh:\n  max_tokens: 1024\n")
		path := filepath.Join(dir, "rysh.config.yaml")
		target := filepath.Join(dir, "ws")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}

		if err := SetWorkspaceWorkingDir(path, 0, "default", target); err != nil {
			t.Fatalf("SetWorkspaceWorkingDir: %v", err)
		}
		cfg, err := loadFrom("", new(strings.Builder))
		if err != nil {
			t.Fatalf("loadFrom: %v", err)
		}
		if cfg.WorkingDirectory != target {
			t.Errorf("rysh.working_directory = %q, want %q", cfg.WorkingDirectory, target)
		}
	})
}

// TestInteractiveShellDefaultsTrue verifies that interactive_shell defaults to
// true when it is not present in the config file.
func TestInteractiveShellDefaultsTrue(t *testing.T) {
	t.Setenv("RYSH_INTERACTIVE_SHELL", "") // guard against a polluted environment
	writeConfig(t, "session:\n  name: \"no-ui-key\"\n")
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom returned error: %v", err)
	}
	if !cfg.InteractiveShell {
		t.Errorf("InteractiveShell = false, want true (default when unset)")
	}
}

// TestInteractiveShellExplicitFalse verifies that interactive_shell: false in
// the config file disables the interactive shell.
func TestInteractiveShellExplicitFalse(t *testing.T) {
	t.Setenv("RYSH_INTERACTIVE_SHELL", "")
	writeConfig(t, "ui:\n  interactive_shell: false\n")
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom returned error: %v", err)
	}
	if cfg.InteractiveShell {
		t.Errorf("InteractiveShell = true, want false (explicit interactive_shell: false)")
	}
}

// TestInteractiveShellEnvOverride verifies RYSH_INTERACTIVE_SHELL overrides the
// config-file value.
func TestInteractiveShellEnvOverride(t *testing.T) {
	t.Setenv("RYSH_INTERACTIVE_SHELL", "false")
	writeConfig(t, "ui:\n  interactive_shell: true\n")
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom returned error: %v", err)
	}
	if cfg.InteractiveShell {
		t.Errorf("InteractiveShell = true, want false (RYSH_INTERACTIVE_SHELL=false overrides file)")
	}
}

// TestAutomationSection verifies the `automation` umbrella parses into
// Config.Automation.Web.Step (config-level defaults for ##auto web recipes),
// including the nested budget.watch.floor block, and that an absent section
// leaves Step nil (built-in defaults apply downstream).
func TestAutomationSection(t *testing.T) {
	writeConfig(t, `
automation:
  web:
    step:
      interval: 30
      max_iterations: 300
      max_duration: 7m
      auto_continue: true
      auto_approve: true
      budget:
        size: 3b
        watch:
          takeover_when: 90
          floor:
            trigger_iterations: 50
            trigger_duration: 1m
            trigger_size: 60p
            iterations: 100
            duration: 5m
            size: 1b
`)
	cfg, err := load(new(strings.Builder))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := cfg.Automation.Web.Step
	if s == nil {
		t.Fatal("automation.web.step not parsed")
	}
	if s.Interval != 30 || s.MaxIterations != 300 || s.MaxDuration != "7m" {
		t.Errorf("step fields: %+v", s)
	}
	if s.AutoContinue == nil || !*s.AutoContinue || s.AutoApprove == nil || !*s.AutoApprove {
		t.Errorf("step bools: %+v", s)
	}
	if s.Budget == nil || s.Budget.Size != "3b" || s.Budget.Watch == nil || s.Budget.Watch.TakeoverWhen != 90 {
		t.Errorf("budget/watch: %+v", s.Budget)
	}
	fl := s.Budget.Watch.Floor
	if fl == nil || fl.TriggerIterations != 50 || fl.TriggerDuration != "1m" || fl.TriggerSize != "60p" ||
		fl.Iterations != 100 || fl.Duration != "5m" || fl.Size != "1b" {
		t.Errorf("floor: %+v", fl)
	}

	// The sibling kinds (automation.task/agent/humanoid.step) parse into their
	// own step blocks, independently of web.
	writeConfig(t, `
automation:
  task:
    step: {interval: 11, max_duration: 3m}
  agent:
    step: {interval: 12}
  humanoid:
    step: {interval: 13}
  code:
    step: {interval: 14}
`)
	cfg, err = load(new(strings.Builder))
	if err != nil {
		t.Fatalf("load (kind siblings): %v", err)
	}
	if s := cfg.Automation.Task.Step; s == nil || s.Interval != 11 || s.MaxDuration != "3m" {
		t.Errorf("automation.task.step not parsed: %+v", s)
	}
	if s := cfg.Automation.Agent.Step; s == nil || s.Interval != 12 {
		t.Errorf("automation.agent.step not parsed: %+v", s)
	}
	if s := cfg.Automation.Humanoid.Step; s == nil || s.Interval != 13 {
		t.Errorf("automation.humanoid.step not parsed: %+v", s)
	}
	if s := cfg.Automation.Code.Step; s == nil || s.Interval != 14 {
		t.Errorf("automation.code.step not parsed: %+v", s)
	}

	// automation.<kind>.loop parses the {do, while} defaults.
	writeConfig(t, `
automation:
  task:
    loop:
      do: {interval: 21}
      while:
        enabled: true
        max_iterations: 3
        max_duration: 30m
        budget: 9b
        prompts: {until: config-level until}
`)
	cfg, err = load(new(strings.Builder))
	if err != nil {
		t.Fatalf("load (loop): %v", err)
	}
	l := cfg.Automation.Task.Loop
	if l == nil || l.Do == nil || l.Do.Interval != 21 {
		t.Fatalf("automation.task.loop.do not parsed: %+v", l)
	}
	if l.While == nil || l.While.Enabled == nil || !*l.While.Enabled ||
		l.While.MaxIterations != 3 || l.While.MaxDuration != "30m" || l.While.Budget != "9b" ||
		l.While.Prompts == nil || l.While.Prompts.Until != "config-level until" {
		t.Errorf("automation.task.loop.while not parsed: %+v", l.While)
	}
	// Unconfigured sibling kinds still get the model-seat defaults folded in
	// (applyAutomationLLMDefaults) — and NOTHING else.
	if s := cfg.Automation.Web.Step; s == nil || s.Model != webauto.DefaultStepModel ||
		s.Interval != 0 || s.Budget != nil || s.AutoContinue != nil {
		t.Errorf("web step should carry only the model-seat default: %+v", s)
	}

	// Absent section → every kind still resolves its two model seats
	// (internal loop → DefaultStepModel, judge → DefaultJudgeModel), with all
	// other fields zero so per-field resolution behavior is unchanged.
	writeConfig(t, "ui:\n  initial_tabs: 2\n")
	cfg, err = load(new(strings.Builder))
	if err != nil {
		t.Fatalf("load (no automation): %v", err)
	}
	for name, k := range map[string]AutomationKindConfig{
		"web": cfg.Automation.Web, "task": cfg.Automation.Task, "agent": cfg.Automation.Agent,
		"humanoid": cfg.Automation.Humanoid, "code": cfg.Automation.Code,
	} {
		if k.Step == nil || k.Step.Model != webauto.DefaultStepModel || k.Step.Interval != 0 {
			t.Errorf("%s: step model seat = %+v, want bare %s", name, k.Step, webauto.DefaultStepModel)
		}
		if k.Loop == nil || k.Loop.While == nil || k.Loop.While.Model != webauto.DefaultJudgeModel ||
			k.Loop.While.Enabled != nil || k.Loop.While.Prompts != nil {
			t.Errorf("%s: judge model seat = %+v, want bare %s", name, k.Loop, webauto.DefaultJudgeModel)
		}
	}
}
