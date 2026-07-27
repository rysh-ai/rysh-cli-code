package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitSpec(t *testing.T) {
	cases := []struct {
		spec, name, version string
		wantErr             bool
	}{
		{spec: "@rysh/code-reviewer", name: "@rysh/code-reviewer"},
		{spec: "@rysh/code-reviewer@0.2.0", name: "@rysh/code-reviewer", version: "0.2.0"},
		// The name itself begins with '@', so the version separator must be the
		// LAST one — splitting on the first would yield an empty name.
		{spec: "@acme/deep-name@1.0.0-rc1", name: "@acme/deep-name", version: "1.0.0-rc1"},
		{spec: "@rysh/x@", wantErr: true},
		{spec: "@rysh", wantErr: true},
		{spec: "@rysh/", wantErr: true},
		{spec: "@a/b/c", wantErr: true},
		{spec: "./local/dir", wantErr: true},
	}
	for _, tc := range cases {
		name, version, err := SplitSpec(tc.spec)
		if tc.wantErr {
			if err == nil {
				t.Errorf("SplitSpec(%q) = (%q,%q), want error", tc.spec, name, version)
			}
			continue
		}
		if err != nil {
			t.Errorf("SplitSpec(%q): %v", tc.spec, err)
			continue
		}
		if name != tc.name || version != tc.version {
			t.Errorf("SplitSpec(%q) = (%q,%q), want (%q,%q)", tc.spec, name, version, tc.name, tc.version)
		}
	}
}

// Version selection must be numeric, not lexical: "0.10.0" is newer than
// "0.9.0" even though it sorts earlier as a string.
func TestResolvePicksHighestVersionNumerically(t *testing.T) {
	idx := &Index{
		SchemaVersion: IndexSchemaVersion,
		Packages: map[string]IndexPackage{
			"@rysh/code-reviewer": {Versions: map[string]IndexVersion{
				"0.9.0":  {URL: "a.tar.gz", Checksum: "sha256:aa"},
				"0.10.0": {URL: "b.tar.gz", Checksum: "sha256:bb"},
				"0.2.0":  {URL: "c.tar.gz", Checksum: "sha256:cc"},
			}},
		},
	}
	_, version, art, err := idx.Resolve("@rysh/code-reviewer")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if version != "0.10.0" {
		t.Fatalf("resolved version = %q, want 0.10.0 (lexical sort would pick 0.9.0)", version)
	}
	if art.URL != "b.tar.gz" {
		t.Fatalf("artifact = %q, want b.tar.gz", art.URL)
	}
}

func TestResolveErrors(t *testing.T) {
	idx := &Index{
		SchemaVersion: IndexSchemaVersion,
		Packages: map[string]IndexPackage{
			"@rysh/code-reviewer": {Versions: map[string]IndexVersion{"1.0.0": {URL: "a.tar.gz", Checksum: "sha256:aa"}}},
			"@rysh/pr-writer":     {Versions: map[string]IndexVersion{"1.0.0": {URL: "b.tar.gz", Checksum: "sha256:bb"}}},
		},
	}
	if _, _, _, err := idx.Resolve("@rysh/nope"); err == nil {
		t.Fatal("expected an error for an unknown package")
	} else if !strings.Contains(err.Error(), "@rysh/code-reviewer") {
		// The suggestion is the point: a typo'd name should show neighbours.
		t.Fatalf("expected same-namespace suggestions, got: %v", err)
	}
	if _, _, _, err := idx.Resolve("@rysh/code-reviewer@9.9.9"); err == nil {
		t.Fatal("expected an error for an unpublished version")
	} else if !strings.Contains(err.Error(), "1.0.0") {
		t.Fatalf("expected the published versions listed, got: %v", err)
	}
}

// An index from a newer rysh must be refused loudly rather than half-parsed.
func TestFetchIndexRejectsUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "index.json")
	body, _ := json.Marshal(map[string]any{
		"schema_version": IndexSchemaVersion + 1,
		"packages":       map[string]any{"@rysh/x": map[string]any{"versions": map[string]any{}}},
	})
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchIndex(p, 0); err == nil {
		t.Fatal("expected a schema-version error")
	} else if !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("error should name the schema mismatch, got: %v", err)
	}
}

func TestAbsoluteURL(t *testing.T) {
	const idxURL = "https://packages.rysh.ai/registry/index.json"
	cases := map[string]string{
		"pkg/a.tar.gz":                   "https://packages.rysh.ai/registry/pkg/a.tar.gz",
		"./pkg/a.tar.gz":                 "https://packages.rysh.ai/registry/pkg/a.tar.gz",
		"https://cdn.example/a.tar.gz":   "https://cdn.example/a.tar.gz",
		"http://internal.local/a.tar.gz": "http://internal.local/a.tar.gz",
	}
	for in, want := range cases {
		if got := AbsoluteURL(idxURL, in); got != want {
			t.Errorf("AbsoluteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// buildPkgTarball produces a minimal, valid package archive.
func buildPkgTarball(t *testing.T, name, version string) []byte {
	t.Helper()
	entry := "SKILL.md"
	skill := "---\nname: " + InstallName(name) + "\n---\n\nYou are a test agent.\n"
	manifest := "name: \"" + name + "\"\nversion: \"" + version + "\"\ntype: agent\n" +
		"entry: " + entry + "\nchecksum: " + Sha256Hex([]byte(skill)) + "\n"

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	root := "pkg/"
	for _, f := range []struct{ name, body string }{
		{root + ManifestFile, manifest},
		{root + entry, skill},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The whole point of the index checksum: a tampered artifact must be refused,
// and nothing may be extracted from it.
func TestInstallFromIndexRefusesTamperedArtifact(t *testing.T) {
	tarball := buildPkgTarball(t, "@rysh/code-reviewer", "1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	ryshDir := t.TempDir()
	wrong := "sha256:" + strings.Repeat("00", 32)

	_, _, err := InstallFromIndex(srv.URL+"/a.tar.gz", wrong, ryshDir, Options{Yes: true, Force: true})
	if err == nil {
		t.Fatal("expected a checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("wrong error: %v", err)
	}
	// Force must NOT override an index checksum failure — that flag is for
	// consent warnings, not for installing bytes that are not what the registry
	// said they would be.
	entries, _ := os.ReadDir(filepath.Join(ryshDir, "agents"))
	if len(entries) != 0 {
		t.Fatalf("SECURITY: %d entries were extracted from an artifact that failed verification", len(entries))
	}
}

// A missing checksum is refused outright rather than warned about: a
// name-based install is precisely where a warning would be clicked through.
func TestInstallFromIndexRefusesMissingChecksum(t *testing.T) {
	_, _, err := InstallFromIndex("https://example.invalid/a.tar.gz", "", t.TempDir(), Options{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("expected a missing-checksum refusal, got: %v", err)
	}
}

// The happy path, end to end: index on disk, artifact over HTTP, package
// installed and spawnable by name.
func TestInstallFromIndexHappyPath(t *testing.T) {
	const pkg = "@rysh/code-reviewer"
	tarball := buildPkgTarball(t, pkg, "1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	idx := Index{
		SchemaVersion: IndexSchemaVersion,
		Packages: map[string]IndexPackage{
			pkg: {Type: "agent", Versions: map[string]IndexVersion{
				"1.0.0": {URL: srv.URL + "/a.tar.gz", Checksum: Sha256Hex(tarball)},
			}},
		},
	}
	body, _ := json.Marshal(idx)
	idxPath := filepath.Join(t.TempDir(), "index.json")
	if err := os.WriteFile(idxPath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := FetchIndex(idxPath, 0)
	if err != nil {
		t.Fatalf("FetchIndex: %v", err)
	}
	name, version, art, err := loaded.Resolve(pkg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if name != pkg || version != "1.0.0" {
		t.Fatalf("resolved %s@%s, want %s@1.0.0", name, version, pkg)
	}

	ryshDir := t.TempDir()
	m, _, err := InstallFromIndex(art.URL, art.Checksum, ryshDir, Options{Yes: true})
	if err != nil {
		t.Fatalf("InstallFromIndex: %v", err)
	}
	if m.Name != pkg {
		t.Fatalf("manifest name = %q, want %q", m.Name, pkg)
	}
	// The installed name is what `##agent spawn <name>` takes.
	installed := filepath.Join(ryshDir, "agents", InstallName(pkg), "SKILL.md")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("package not installed where ##agent spawn expects it (%s): %v", installed, err)
	}
}
