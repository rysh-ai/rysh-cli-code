package registry

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// seedPackagesDir is the checked-in first-party package source (design 005
// G4). These tests are the "all 10 install" gate: every seed package must
// parse, install cleanly, and — the consent gate's honesty condition — declare
// exactly the capabilities its skill file actually references.
const seedPackagesDir = "../../packages"

// seedPackageDirs returns the package directories under packages/ (sorted).
func seedPackageDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(seedPackagesDir)
	if err != nil {
		t.Fatalf("read %s: %v", seedPackagesDir, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	return dirs
}

// TestSeedPackagesInstall installs every checked-in package into one fresh
// .rysh tree: consent summary renders, files land under the type's subdir,
// and the lockfile records all of them.
func TestSeedPackagesInstall(t *testing.T) {
	dirs := seedPackageDirs(t)
	if len(dirs) < 10 {
		t.Fatalf("design 005 G4 seeds 10 first-party packages, found %d: %v", len(dirs), dirs)
	}

	ryshDir := t.TempDir()
	for _, name := range dirs {
		src := filepath.Join(seedPackagesDir, name)
		// Yes (not Force): env-declaring packages are Sensitive and must
		// install with plain consent, no override.
		m, warnings, err := InstallFromDir(src, ryshDir, Options{Yes: true})
		if err != nil {
			t.Errorf("%s: install failed: %v", name, err)
			continue
		}
		// Seed manifests carry no hand-written checksum (registry-index stamps
		// it), so exactly the no-checksum warning is expected.
		if len(warnings) != 1 || warnings[0] != "package has no checksum" {
			t.Errorf("%s: warnings = %v, want only the no-checksum warning", name, warnings)
		}
		if m.Name != "@rysh/"+name {
			t.Errorf("%s: manifest name %q, want %q", name, m.Name, "@rysh/"+name)
		}
		td, err := TypeDir(m.Type)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		entry := filepath.Join(ryshDir, td, name, m.Entry)
		if _, err := os.Stat(entry); err != nil {
			t.Errorf("%s: installed entry missing: %v", name, err)
		}
		summary := m.ConsentSummary()
		for _, want := range append(append(append([]string{}, m.DeclaresTools...),
			m.DeclaresChannels...), m.DeclaresEnv...) {
			if !strings.Contains(summary, want) {
				t.Errorf("%s: consent summary omits declared capability %q:\n%s", name, want, summary)
			}
		}
	}

	lock, err := LoadLock(ryshDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(lock.InstalledNames()); got != len(dirs) {
		t.Errorf("lockfile has %d packages, want %d (%v)", got, len(dirs), lock.InstalledNames())
	}
}

// knownSeedTools is the vocabulary seed packages may declare: names as
// registered by the agentic tool registry (internal/tools + browser_action
// from internal/agentic). A declared tool outside this set is a typo or a
// capability rysh cannot actually grant — either way the consent gate would
// render fiction.
var knownSeedTools = map[string]bool{
	"bash": true, "file_read": true, "file_write": true, "edit": true,
	"grep": true, "glob": true, "symbol_search": true, "test_run": true,
	"git_commit": true, "web_fetch": true, "web_search": true,
	"browser_action": true, "env_read": true, "todo": true,
}

var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// TestSeedPackagesDeclareTruthfully pins the consent gate to reality:
//   - declares_env must equal the set of ${VAR} references in the entry file
//     (nothing undeclared, nothing declared-but-unused);
//   - humanoids must declare channels and each declared channel must exist as
//     a contacts: key in the skill frontmatter; non-humanoids declare none;
//   - recipes drive the browser, so they must declare browser_action;
//   - every declared tool must name a real rysh tool.
func TestSeedPackagesDeclareTruthfully(t *testing.T) {
	for _, name := range seedPackageDirs(t) {
		src := filepath.Join(seedPackagesDir, name)
		manData, err := os.ReadFile(filepath.Join(src, ManifestFile))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		m, err := ParseManifest(manData)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if m.Description == "" {
			t.Errorf("%s: manifest needs a description (rendered in the index)", name)
		}
		entryData, err := os.ReadFile(filepath.Join(src, m.Entry))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		entry := string(entryData)

		// env truth, both directions.
		referenced := map[string]bool{}
		for _, match := range envRefPattern.FindAllStringSubmatch(entry, -1) {
			referenced[match[1]] = true
		}
		declared := map[string]bool{}
		for _, v := range m.DeclaresEnv {
			declared[v] = true
			if !referenced[v] {
				t.Errorf("%s: declares_env %q but the entry never references ${%s}", name, v, v)
			}
		}
		for v := range referenced {
			if !declared[v] {
				t.Errorf("%s: entry references ${%s} but declares_env omits it", name, v)
			}
		}

		// channel truth.
		if m.Type == "humanoid" {
			if len(m.DeclaresChannels) == 0 {
				t.Errorf("%s: a humanoid must declare its channels", name)
			}
			for _, ch := range m.DeclaresChannels {
				if !strings.Contains(entry, ch+":") {
					t.Errorf("%s: declares channel %q but contacts has no %s: block", name, ch, ch)
				}
			}
		} else if len(m.DeclaresChannels) > 0 {
			t.Errorf("%s: only humanoids talk on channels; declares_channels = %v", name, m.DeclaresChannels)
		}

		// tool truth.
		if m.Type == "recipe" {
			found := false
			for _, tool := range m.DeclaresTools {
				if tool == "browser_action" {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: a web recipe drives the browser; declare browser_action", name)
			}
		}
		for _, tool := range m.DeclaresTools {
			if !knownSeedTools[tool] {
				t.Errorf("%s: declared tool %q is not a rysh tool name", name, tool)
			}
		}
	}
}
