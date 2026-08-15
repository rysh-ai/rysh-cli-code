// SPDX-License-Identifier: Apache-2.0

// Package registry implements the client side of the rysh agent registry
// (design 005): install .md agents / loops / recipes / humanoids into a
// project's .rysh/ tree, reproducibly (lockfile) and with install-time consent
// (declared tools / channels / env are shown before anything is written).
//
// Format is open, service is private (npm model). This package is the open
// client. The manifest is `rysh.pkg.yaml` (YAML for consistency with the rest
// of rysh's config; the design's TOML sketch is translated to YAML here).
package registry

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ManifestFile is the per-package manifest name.
const ManifestFile = "rysh.pkg.yaml"

// LockFile is the reproducibility lockfile, written under .rysh/.
const LockFile = "rysh.lock"

// Manifest describes a package (open spec).
type Manifest struct {
	Name             string   `yaml:"name"`              // e.g. "@rysh/code-reviewer"
	Version          string   `yaml:"version"`           // semver
	Type             string   `yaml:"type"`              // agent|loop|recipe|humanoid
	Description      string   `yaml:"description"`       // one-liner for search/index page
	Entry            string   `yaml:"entry"`             // primary file, e.g. "SKILL.md"
	MinRysh          string   `yaml:"min_rysh"`          // minimum rysh version
	Checksum         string   `yaml:"checksum"`          // sha256:... over Entry
	DeclaresTools    []string `yaml:"declares_tools"`    // tools the skill uses
	DeclaresChannels []string `yaml:"declares_channels"` // humanoid channels
	DeclaresEnv      []string `yaml:"declares_env"`      // ${ENV} vars referenced
	Signature        string   `yaml:"signature"`         // detached sig (verified namespaces)
}

// ParseManifest parses and validates a manifest.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Name == "" || m.Version == "" || m.Type == "" || m.Entry == "" {
		return nil, fmt.Errorf("manifest: name, version, type, entry are required")
	}
	if _, err := TypeDir(m.Type); err != nil {
		return nil, err
	}
	return &m, nil
}

// TypeDir maps a package type to its .rysh subdirectory.
func TypeDir(t string) (string, error) {
	switch t {
	case "agent":
		return "agents", nil
	case "loop":
		return "loops", nil
	case "recipe":
		return filepath.Join("automations", "webs"), nil
	case "humanoid":
		return "humanoids", nil
	default:
		return "", fmt.Errorf("unknown package type %q (want agent|loop|recipe|humanoid)", t)
	}
}

// InstallName is the on-disk directory name for a (possibly namespaced) package
// name: "@rysh/code-reviewer" → "code-reviewer".
func InstallName(pkgName string) string {
	name := pkgName
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Sensitive reports whether a package requests high-trust capabilities that
// warrant explicit confirmation: it wants to commit code or read secrets.
//
// The Signature field deliberately does NOT downgrade this. Nothing in rysh
// verifies signatures yet, so treating a non-empty signature string as "trusted
// tier" let any attacker-authored manifest skip the consent gate by writing an
// arbitrary value — a malicious package got a weaker check than an honest one.
// Until signature verification exists (design 005 G3), every package is graded
// on its declared capabilities alone.
func (m *Manifest) Sensitive() bool {
	for _, t := range m.DeclaresTools {
		if t == "git_commit" {
			return true
		}
	}
	return len(m.DeclaresEnv) > 0
}

// ConsentSummary renders the declared capabilities for an install-time prompt.
func (m *Manifest) ConsentSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Package: %s@%s (%s)\n", m.Name, m.Version, m.Type)
	if len(m.DeclaresTools) > 0 {
		fmt.Fprintf(&b, "  tools:    %s\n", strings.Join(m.DeclaresTools, ", "))
	}
	if len(m.DeclaresChannels) > 0 {
		fmt.Fprintf(&b, "  channels: %s\n", strings.Join(m.DeclaresChannels, ", "))
	}
	if len(m.DeclaresEnv) > 0 {
		fmt.Fprintf(&b, "  env:      %s\n", strings.Join(m.DeclaresEnv, ", "))
	}
	sig := "unsigned (checksum-only)"
	if m.Signature != "" {
		// Never render this as plain "signed": no signature verification is
		// implemented, so the field is an unverified author claim.
		sig = "signature present but UNVERIFIED (checksum-only)"
	}
	fmt.Fprintf(&b, "  trust:    %s\n", sig)
	return b.String()
}

// LockEntry records an installed package for reproducibility.
type LockEntry struct {
	Version   string `yaml:"version"`
	Type      string `yaml:"type"`
	Checksum  string `yaml:"checksum"`
	Path      string `yaml:"path"`
	Installed string `yaml:"installed"`
}

// Lock is the .rysh/rysh.lock document (name → entry).
type Lock struct {
	Packages map[string]LockEntry `yaml:"packages"`
}

// LoadLock reads the lockfile from ryshDir (empty lock if absent).
func LoadLock(ryshDir string) (*Lock, error) {
	data, err := os.ReadFile(filepath.Join(ryshDir, LockFile))
	if os.IsNotExist(err) {
		return &Lock{Packages: map[string]LockEntry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := yaml.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("parse lockfile: %w", err)
	}
	if l.Packages == nil {
		l.Packages = map[string]LockEntry{}
	}
	return &l, nil
}

// Save writes the lockfile deterministically (map keys are sorted by yaml.v3).
func (l *Lock) Save(ryshDir string) error {
	if err := os.MkdirAll(ryshDir, 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(l)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(ryshDir, LockFile), data, 0o644)
}

// InstalledNames returns the sorted set of installed package names.
func (l *Lock) InstalledNames() []string {
	out := make([]string, 0, len(l.Packages))
	for k := range l.Packages {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Sha256Hex returns "sha256:<hex>" over data.
func Sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Options controls an install.
type Options struct {
	Yes   bool // proceed past the consent prompt non-interactively
	Force bool // proceed despite sensitive/unsigned/checksum warnings
}

// InstallFromDir installs the package rooted at srcDir into ryshDir, returning
// the manifest and any warnings. It verifies the entry checksum, refuses a
// sensitive/unsigned package unless Force, and updates the lockfile.
func InstallFromDir(srcDir, ryshDir string, opts Options) (*Manifest, []string, error) {
	manData, err := os.ReadFile(filepath.Join(srcDir, ManifestFile))
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	m, err := ParseManifest(manData)
	if err != nil {
		return nil, nil, err
	}

	var warnings []string

	// Verify the entry checksum when the manifest declares one.
	entryPath := filepath.Join(srcDir, m.Entry)
	entryData, err := os.ReadFile(entryPath)
	if err != nil {
		return m, nil, fmt.Errorf("read entry %q: %w", m.Entry, err)
	}
	if m.Checksum != "" {
		got := Sha256Hex(entryData)
		if !strings.EqualFold(got, m.Checksum) {
			if !opts.Force {
				return m, nil, fmt.Errorf("checksum mismatch for %s: manifest=%s got=%s (use --force to override)", m.Entry, m.Checksum, got)
			}
			warnings = append(warnings, "checksum mismatch overridden by --force")
		}
	} else {
		warnings = append(warnings, "package has no checksum")
	}

	if m.Sensitive() && !opts.Force && !opts.Yes {
		return m, warnings, fmt.Errorf("package requests sensitive capabilities (git_commit and/or env secrets) and is unsigned; re-run with --yes to accept or --force")
	}

	typeDir, _ := TypeDir(m.Type)
	destRel := filepath.Join(typeDir, InstallName(m.Name))
	dest := filepath.Join(ryshDir, destRel)
	if err := copyTree(srcDir, dest); err != nil {
		return m, warnings, fmt.Errorf("install files: %w", err)
	}

	lock, err := LoadLock(ryshDir)
	if err != nil {
		return m, warnings, err
	}
	lock.Packages[m.Name] = LockEntry{
		Version:   m.Version,
		Type:      m.Type,
		Checksum:  m.Checksum,
		Path:      destRel,
		Installed: nowStamp(),
	}
	if err := lock.Save(ryshDir); err != nil {
		return m, warnings, fmt.Errorf("write lockfile: %w", err)
	}
	return m, warnings, nil
}

// InstallFromTarball downloads/opens a .tar.gz (URL or local path), extracts it
// to a temp dir, and installs it. The archive must contain the manifest at its
// root (or in a single top-level directory).
func InstallFromTarball(src, ryshDir string, opts Options) (*Manifest, []string, error) {
	var rc io.ReadCloser
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		resp, err := http.Get(src) //nolint:gosec // user-supplied registry URL
		if err != nil {
			return nil, nil, fmt.Errorf("fetch %s: %w", src, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, nil, fmt.Errorf("fetch %s: status %d", src, resp.StatusCode)
		}
		rc = resp.Body
	} else {
		f, err := os.Open(src)
		if err != nil {
			return nil, nil, err
		}
		rc = f
	}
	defer func() { _ = rc.Close() }()

	tmp, err := os.MkdirTemp("", "rysh-pkg-*")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	root, err := extractTarGz(rc, tmp)
	if err != nil {
		return nil, nil, err
	}
	return InstallFromDir(root, ryshDir, opts)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// extractTarGz extracts into destDir and returns the directory that contains
// the manifest (destDir itself, or the single top-level dir).
func extractTarGz(r io.Reader, destDir string) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		// Guard against path traversal.
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return "", fmt.Errorf("unsafe path in archive: %q", hdr.Name)
		}
		target := filepath.Join(destDir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			f, err := os.Create(target)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // bounded by archive
				_ = f.Close()
				return "", err
			}
			_ = f.Close()
		}
	}
	// Manifest at root?
	if _, err := os.Stat(filepath.Join(destDir, ManifestFile)); err == nil {
		return destDir, nil
	}
	// Otherwise a single top-level directory.
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(destDir, e.Name(), ManifestFile)); err == nil {
				return filepath.Join(destDir, e.Name()), nil
			}
		}
	}
	return "", fmt.Errorf("no %s found in archive", ManifestFile)
}

// nowStamp is overridable in tests; wall-clock RFC3339 by default.
var nowStamp = func() string { return time.Now().UTC().Format(time.RFC3339) }
