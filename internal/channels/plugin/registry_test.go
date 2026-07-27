package plugin

// Registry + factory resolution tests (design 002 §4.4/§6): an installed
// plugin resolves through channels.NewAdapter's default path; built-ins
// always win; IsValidChannelType accepts installed plugin names.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// writePluginDir lays out an installed-plugin (or package source) directory.
func writePluginDir(t *testing.T, dir string, manifest string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func mockchanManifestTOML(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("name = %q\nversion = %q\ntransport = %q\nexec = %q\n",
		"mockchan", "0.0.1", "stdio", exe+" -test.run=TestHelperPluginProcess")
}

func TestRegistryLookupAndList(t *testing.T) {
	root := t.TempDir()
	writePluginDir(t, filepath.Join(root, "mockchan"), mockchanManifestTOML(t), nil)
	// A hand-planted "slack" plugin dir: never resolvable.
	writePluginDir(t, filepath.Join(root, "slack"),
		"name = \"slack\"\ntransport = \"stdio\"\nexec = \"run\"\n", map[string]string{"run": "#!/bin/sh\n"})
	// A dir whose manifest name disagrees with the dir name: ignored.
	writePluginDir(t, filepath.Join(root, "otherdir"),
		"name = \"mismatch\"\ntransport = \"stdio\"\nexec = \"run\"\n", nil)

	reg, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if _, ok := reg.Lookup("mockchan"); !ok {
		t.Fatal("mockchan should resolve")
	}
	if _, ok := reg.Lookup("slack"); ok {
		t.Fatal("a plugin named slack must never resolve (built-in collision)")
	}
	if _, ok := reg.Lookup("otherdir"); ok {
		t.Fatal("dir/manifest name mismatch must not resolve")
	}
	if _, ok := reg.Lookup("missing"); ok {
		t.Fatal("missing plugin must not resolve")
	}

	list := reg.List()
	if len(list) != 1 || list[0].Manifest.Name != "mockchan" {
		t.Fatalf("List = %+v, want just mockchan", list)
	}
}

func TestLoadRegistryMissingRootIsEmpty(t *testing.T) {
	reg, err := LoadRegistry(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("LoadRegistry on missing root: %v", err)
	}
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("List on missing root = %+v", got)
	}
}

// TestFactoryResolvesInstalledPlugin wires the hooks and drives the REAL
// channels.NewAdapter default path, including an end-to-end Start/inbound
// round-trip through the factory-built adapter.
func TestFactoryResolvesInstalledPlugin(t *testing.T) {
	root := t.TempDir()
	writePluginDir(t, filepath.Join(root, "mockchan"), mockchanManifestTOML(t), nil)
	// Shadow attempt: ignored by install-time rules AND factory order.
	writePluginDir(t, filepath.Join(root, "slack"),
		"name = \"slack\"\ntransport = \"stdio\"\nexec = \"run\"\n", map[string]string{"run": "#!/bin/sh\n"})

	Wire(WireOptions{Root: root, Supervisor: testOpts("MOCK_TRANSPORT=stdio")})
	defer Unwire()

	// IsValidChannelType: built-ins + installed plugins, nothing else.
	if !channels.IsValidChannelType("mockchan") {
		t.Fatal("IsValidChannelType must accept an installed plugin")
	}
	if !channels.IsValidChannelType("slack") {
		t.Fatal("built-in types must stay valid")
	}
	if channels.IsValidChannelType("not-installed") {
		t.Fatal("unknown types must stay invalid")
	}

	// Built-ins always win: "slack" resolves to the built-in adapter even
	// with a slack-named plugin dir on disk.
	slackAdapter, err := channels.NewAdapter("slack", msg.ChannelConfig{})
	if err != nil {
		t.Fatalf("NewAdapter(slack): %v", err)
	}
	if _, isBuiltin := slackAdapter.(*channels.SlackAdapter); !isBuiltin {
		t.Fatalf("NewAdapter(slack) = %T, want the built-in *channels.SlackAdapter", slackAdapter)
	}

	// The plugin resolves through the default case.
	adapter, err := channels.NewAdapter("mockchan", msg.ChannelConfig{Enabled: true})
	if err != nil {
		t.Fatalf("NewAdapter(mockchan): %v", err)
	}
	pa, ok := adapter.(*PluginChannelAdapter)
	if !ok {
		t.Fatalf("NewAdapter(mockchan) = %T, want *PluginChannelAdapter", adapter)
	}
	if pa.Type() != "mockchan" {
		t.Fatalf("Type() = %q", pa.Type())
	}

	// And it is a working ChannelAdapter end to end.
	if err := pa.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer pa.Stop()
	recvInboundWith(t, pa.InboundCh(), 5*time.Second, "hello via factory-built adapter",
		func(im channels.InboundMessage) bool { return im.Content == "hello-from-plugin" })

	// Unknown types still fail with the factory's error.
	if _, err := channels.NewAdapter("no-such-plugin", msg.ChannelConfig{}); err == nil ||
		!strings.Contains(err.Error(), "unsupported channel type") {
		t.Fatalf("unknown type error = %v", err)
	}
}

// TestUnwiredHooksAreNilSafe: without Wire, plugin types are simply unknown.
func TestUnwiredHooksAreNilSafe(t *testing.T) {
	Unwire()
	if channels.IsValidChannelType("mockchan") {
		t.Fatal("unwired IsValidChannelType must not accept plugin names")
	}
	if _, err := channels.NewAdapter("mockchan", msg.ChannelConfig{}); err == nil {
		t.Fatal("unwired NewAdapter must reject plugin types")
	}
}

// --- install flow -----------------------------------------------------------

// buildTarball packs files (path → content) into a .tar.gz and returns its
// path and checksum.
func buildTarball(t *testing.T, dir string, files map[string]string) (path, checksum string) {
	t.Helper()
	path = filepath.Join(dir, "pkg.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return path, "sha256:" + hex.EncodeToString(sum[:])
}

const testPkgManifest = "name = \"pkgchan\"\nversion = \"1.0.0\"\ntransport = \"stdio\"\nexec = \"run.sh\"\n"

func TestInstallFromDir(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "pkgchan-src")
	writePluginDir(t, src, testPkgManifest, map[string]string{"run.sh": "#!/bin/sh\nexit 0\n"})

	p, err := InstallPackage(root, src, "")
	if err != nil {
		t.Fatalf("InstallPackage: %v", err)
	}
	if p.Manifest.Name != "pkgchan" || p.Dir != filepath.Join(root, "pkgchan") {
		t.Fatalf("installed = %+v", p)
	}
	for _, f := range []string{ManifestFileName, "run.sh", InstallRecordFileName} {
		if _, err := os.Stat(filepath.Join(p.Dir, f)); err != nil {
			t.Fatalf("missing %s after install: %v", f, err)
		}
	}
	// Exec is executable.
	st, _ := os.Stat(filepath.Join(p.Dir, "run.sh"))
	if st.Mode()&0o100 == 0 {
		t.Fatalf("exec not executable: %v", st.Mode())
	}
	// Resolvable via the registry.
	reg, _ := LoadRegistry(root)
	if _, ok := reg.Lookup("pkgchan"); !ok {
		t.Fatal("installed plugin not resolvable")
	}

	// Double install refuses.
	if _, err := InstallPackage(root, src, ""); err == nil ||
		!strings.Contains(err.Error(), "already installed") {
		t.Fatalf("double install err = %v", err)
	}

	// Remove deletes the dir.
	if err := Remove(root, "pkgchan"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(p.Dir); !os.IsNotExist(err) {
		t.Fatal("plugin dir still present after Remove")
	}
	if err := Remove(root, "pkgchan"); err == nil {
		t.Fatal("removing a non-installed plugin must error")
	}
}

func TestInstallFromTarballWithChecksum(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"pkgchan/" + ManifestFileName: testPkgManifest,
		"pkgchan/run.sh":              "#!/bin/sh\nexit 0\n",
	}
	pkg, sum := buildTarball(t, t.TempDir(), files)

	// Wrong checksum: rejected before anything lands on disk.
	if _, err := InstallPackage(root, pkg, "sha256:"+strings.Repeat("0", 64)); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("bad checksum err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "pkgchan")); !os.IsNotExist(err) {
		t.Fatal("failed install must not leave files behind")
	}

	// Right checksum: installs, stripping the single top-level dir.
	p, err := InstallPackage(root, pkg, sum)
	if err != nil {
		t.Fatalf("InstallPackage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "run.sh")); err != nil {
		t.Fatalf("run.sh not extracted at plugin root: %v", err)
	}
}

func TestInstallRejectsBuiltinName(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "slack-src")
	writePluginDir(t, src, "name = \"slack\"\ntransport = \"stdio\"\nexec = \"run.sh\"\n",
		map[string]string{"run.sh": "#!/bin/sh\n"})
	if _, err := InstallPackage(root, src, ""); err == nil ||
		!strings.Contains(err.Error(), "collides with a built-in") {
		t.Fatalf("builtin-name install err = %v", err)
	}
}

func TestInstallRejectsTraversalTarball(t *testing.T) {
	root := t.TempDir()
	pkg, _ := buildTarball(t, t.TempDir(), map[string]string{
		ManifestFileName: testPkgManifest,
		"../evil.sh":     "#!/bin/sh\n",
		"run.sh":         "#!/bin/sh\n",
	})
	if _, err := InstallPackage(root, pkg, ""); err == nil ||
		!strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("traversal err = %v", err)
	}
}

func TestInstallRejectsMissingExec(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "noexec-src")
	writePluginDir(t, src, testPkgManifest, nil) // manifest says run.sh, file absent
	if _, err := InstallPackage(root, src, ""); err == nil ||
		!strings.Contains(err.Error(), "not found in package") {
		t.Fatalf("missing exec err = %v", err)
	}
}
