package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func writePkg(t *testing.T, dir, name, entryBody string, tools, env []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(entryBody), 0o644); err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Name: name, Version: "1.0.0", Type: "agent", Entry: "SKILL.md",
		Checksum: Sha256Hex([]byte(entryBody)), DeclaresTools: tools, DeclaresEnv: env,
	}
	man := "name: \"" + m.Name + "\"\nversion: 1.0.0\ntype: agent\nentry: SKILL.md\nchecksum: " + m.Checksum + "\n"
	if len(tools) > 0 {
		man += "declares_tools: [" + tools[0] + "]\n"
	}
	if len(env) > 0 {
		man += "declares_env: [" + env[0] + "]\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFromDir_HappyPath(t *testing.T) {
	src := filepath.Join(t.TempDir(), "pkg")
	writePkg(t, src, "@rysh/code-reviewer", "You are a code reviewer.", []string{"file_read"}, nil)
	ryshDir := t.TempDir()

	m, warnings, err := InstallFromDir(src, ryshDir, Options{})
	if err != nil {
		t.Fatalf("install: %v (warnings %v)", err, warnings)
	}
	if m.Name != "@rysh/code-reviewer" {
		t.Fatalf("name = %q", m.Name)
	}
	// File landed at agents/code-reviewer/SKILL.md.
	if _, err := os.Stat(filepath.Join(ryshDir, "agents", "code-reviewer", "SKILL.md")); err != nil {
		t.Fatalf("installed file missing: %v", err)
	}
	// Lockfile records it.
	lock, err := LoadLock(ryshDir)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := lock.Packages["@rysh/code-reviewer"]
	if !ok || e.Version != "1.0.0" || e.Type != "agent" {
		t.Fatalf("lock entry = %+v ok=%v", e, ok)
	}
}

func TestInstallFromDir_ChecksumMismatch(t *testing.T) {
	src := filepath.Join(t.TempDir(), "pkg")
	writePkg(t, src, "@x/bad", "body", nil, nil)
	// Corrupt the entry after the manifest was written with the old checksum.
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("TAMPERED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InstallFromDir(src, t.TempDir(), Options{}); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	// --force overrides.
	if _, _, err := InstallFromDir(src, t.TempDir(), Options{Force: true}); err != nil {
		t.Fatalf("force install: %v", err)
	}
}

func TestInstallFromDir_SensitiveNeedsConsent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "pkg")
	writePkg(t, src, "@x/committer", "body", []string{"git_commit"}, []string{"GITHUB_TOKEN"})
	// Unsigned + git_commit + env ⇒ sensitive ⇒ blocked without --yes.
	if _, _, err := InstallFromDir(src, t.TempDir(), Options{}); err == nil {
		t.Fatal("expected sensitive-consent error")
	}
	if _, _, err := InstallFromDir(src, t.TempDir(), Options{Yes: true}); err != nil {
		t.Fatalf("consented install: %v", err)
	}
}

func TestInstallFromTarball(t *testing.T) {
	// Build an in-memory package and tar.gz it.
	body := "You are a tarball agent."
	man := "name: \"@rysh/tarred\"\nversion: 2.1.0\ntype: agent\nentry: SKILL.md\nchecksum: " + Sha256Hex([]byte(body)) + "\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range map[string]string{"pkg/" + ManifestFile: man, "pkg/SKILL.md": body} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()

	tarPath := filepath.Join(t.TempDir(), "pkg.tar.gz")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	ryshDir := t.TempDir()
	m, _, err := InstallFromTarball(tarPath, ryshDir, Options{})
	if err != nil {
		t.Fatalf("tarball install: %v", err)
	}
	if m.Version != "2.1.0" {
		t.Fatalf("version = %q", m.Version)
	}
	if _, err := os.Stat(filepath.Join(ryshDir, "agents", "tarred", "SKILL.md")); err != nil {
		t.Fatalf("installed file missing: %v", err)
	}
}
