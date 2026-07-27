package plugin

// Install/remove flow (design 002 §4.4, adapted): v1 installs from a LOCAL
// tarball (.tar.gz/.tgz) or directory — no registry service exists yet;
// fetch-from-a-registry is a documented TODO (design 005's registry client).
// Nothing executes at install time: files are extracted/copied into
// .rysh/channels/{name}/ and an install record is written; the plugin process
// only ever starts when a humanoid starts the channel.

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"
	"time"
)

// InstallRecordFileName is the per-plugin install record.
const InstallRecordFileName = "install.json"

// InstallRecord is written next to the manifest at install time.
type InstallRecord struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Transport   string    `json:"transport"`
	Tier        string    `json:"tier"`
	Source      string    `json:"source"`             // the path installed from
	Checksum    string    `json:"checksum,omitempty"` // verified tarball checksum, when one was checked
	InstalledAt time.Time `json:"installed_at"`
}

// isTarball reports whether path names a gzipped tarball package.
func isTarball(path string) bool {
	return strings.HasSuffix(path, ".tar.gz") || strings.HasSuffix(path, ".tgz")
}

// ReadPackageManifest reads and validates the manifest from a package source
// (directory or tarball) WITHOUT installing anything — the CLI shows the
// consent scan from this before any files land in .rysh/.
func ReadPackageManifest(src string) (Manifest, error) {
	st, err := os.Stat(src)
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin install: %w", err)
	}
	if st.IsDir() {
		return LoadManifest(src)
	}
	if !isTarball(src) {
		return Manifest{}, fmt.Errorf("plugin install: %s is neither a directory nor a .tar.gz/.tgz package", src)
	}
	data, _, err := tarballManifest(src)
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(data)
}

// InstallPackage installs src (dir or tarball) into root/{name}. When
// expectedChecksum is non-empty and src is a tarball, the tarball's sha256 is
// verified first.
//
// Checksum note (v1): the manifest's own `checksum` field describes the
// package tarball for the REGISTRY flow — it cannot be verified against a
// tarball that contains it (self-reference), so local installs verify only an
// out-of-band checksum passed by the caller (`--checksum`).
func InstallPackage(root, src, expectedChecksum string) (InstalledPlugin, error) {
	if root == "" {
		root = DefaultRoot
	}
	st, err := os.Stat(src)
	if err != nil {
		return InstalledPlugin{}, fmt.Errorf("plugin install: %w", err)
	}
	fromDir := st.IsDir()
	if !fromDir && !isTarball(src) {
		return InstalledPlugin{}, fmt.Errorf("plugin install: %s is neither a directory nor a .tar.gz/.tgz package", src)
	}

	verifiedChecksum := ""
	if expectedChecksum != "" {
		if fromDir {
			return InstalledPlugin{}, errors.New("plugin install: --checksum applies to tarball packages, not directories")
		}
		if err := VerifyChecksum(src, expectedChecksum); err != nil {
			return InstalledPlugin{}, err
		}
		verifiedChecksum = expectedChecksum
	}

	m, err := ReadPackageManifest(src)
	if err != nil {
		return InstalledPlugin{}, err
	}

	dest := filepath.Join(root, m.Name)
	if _, err := os.Stat(dest); err == nil {
		return InstalledPlugin{}, fmt.Errorf(progname.Rewrite("plugin install: %q is already installed (rysh channel remove %s first)"), m.Name, m.Name)
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return InstalledPlugin{}, fmt.Errorf("plugin install: %w", err)
	}
	tmp, err := os.MkdirTemp(root, ".install-")
	if err != nil {
		return InstalledPlugin{}, fmt.Errorf("plugin install: %w", err)
	}
	defer os.RemoveAll(tmp) // no-op after the successful rename

	if fromDir {
		err = copyTree(src, tmp)
	} else {
		err = extractTarGz(src, tmp)
	}
	if err != nil {
		return InstalledPlugin{}, err
	}

	// Re-validate from the extracted tree (what we installed is what we
	// scanned) and check the exec target landed.
	installed, err := LoadManifest(tmp)
	if err != nil {
		return InstalledPlugin{}, err
	}
	if installed.Name != m.Name {
		return InstalledPlugin{}, fmt.Errorf("plugin install: manifest name changed between scan (%q) and extraction (%q)", m.Name, installed.Name)
	}
	execBin := strings.Fields(installed.Exec)[0]
	if !filepath.IsAbs(execBin) {
		binPath := filepath.Join(tmp, execBin)
		if !fileExists(binPath) {
			return InstalledPlugin{}, fmt.Errorf("plugin install: exec %q not found in package", execBin)
		}
		if err := os.Chmod(binPath, 0o755); err != nil {
			return InstalledPlugin{}, fmt.Errorf("plugin install: chmod exec: %w", err)
		}
	}

	// Signature check on the extracted tree: a package that ships a broken
	// rysh.channel.sig never installs (same refusal the loader applies);
	// unsigned / unknown-key packages install as before, and the verdict is
	// recorded so `channel list` and the operator see the tier at a glance.
	verdict := VerifyPlugin(tmp, installed, DefaultTrustFile)
	if verdict.Status == SigInvalid {
		return InstalledPlugin{}, fmt.Errorf("plugin install: signature verification failed for %q: %s", installed.Name, verdict.Reason)
	}

	rec := InstallRecord{
		Name:        installed.Name,
		Version:     installed.Version,
		Transport:   installed.Transport,
		Tier:        verdict.TierString(),
		Source:      src,
		Checksum:    verifiedChecksum,
		InstalledAt: time.Now(),
	}
	recData, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return InstalledPlugin{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, InstallRecordFileName), recData, 0o644); err != nil {
		return InstalledPlugin{}, fmt.Errorf("plugin install: write install record: %w", err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		return InstalledPlugin{}, fmt.Errorf("plugin install: %w", err)
	}
	return InstalledPlugin{Manifest: installed, Dir: dest}, nil
}

// Remove deletes an installed plugin's directory.
func Remove(root, name string) error {
	if root == "" {
		root = DefaultRoot
	}
	if !pluginNameRe.MatchString(name) {
		return fmt.Errorf("plugin remove: invalid plugin name %q", name)
	}
	dir := filepath.Join(root, name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("plugin remove: %q is not installed", name)
	}
	return os.RemoveAll(dir)
}

// tarballManifest locates and returns the manifest bytes inside a tarball,
// plus the path prefix ("" or "<topdir>/") every member shares.
func tarballManifest(src string) (data []byte, prefix string, err error) {
	err = walkTarGz(src, func(hdr *tar.Header, r io.Reader) error {
		name := filepath.ToSlash(filepath.Clean(hdr.Name))
		if name == ManifestFileName {
			prefix = ""
		} else if strings.HasSuffix(name, "/"+ManifestFileName) && strings.Count(name, "/") == 1 {
			prefix = name[:strings.Index(name, "/")+1]
		} else {
			return nil
		}
		b, rerr := io.ReadAll(io.LimitReader(r, 1<<20))
		if rerr != nil {
			return rerr
		}
		data = b
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if data == nil {
		return nil, "", fmt.Errorf("plugin install: %s not found at package root", ManifestFileName)
	}
	return data, prefix, nil
}

// walkTarGz streams every regular entry of a gzipped tarball to fn.
func walkTarGz(src string, fn func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("plugin install: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("plugin install: %s: %w", src, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("plugin install: read %s: %w", src, err)
		}
		if err := fn(hdr, tr); err != nil {
			return err
		}
	}
}

// extractTarGz extracts the package into dst, stripping the single top-level
// directory when the manifest lives one level down. Path-traversal-safe:
// absolute names and ".." components are rejected; symlinks are refused.
func extractTarGz(src, dst string) error {
	_, prefix, err := tarballManifest(src)
	if err != nil {
		return err
	}
	return walkTarGz(src, func(hdr *tar.Header, r io.Reader) error {
		name := filepath.ToSlash(filepath.Clean(hdr.Name))
		if name == "." || !strings.HasPrefix(name, prefix) {
			return nil
		}
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" {
			return nil
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
			return fmt.Errorf("plugin install: unsafe path %q in package", hdr.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))

		switch hdr.Typeflag {
		case tar.TypeDir:
			return os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, r); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("plugin install: links are not allowed in packages (%q)", hdr.Name)
		default:
			return nil // ignore other entry types
		}
	})
}

// copyTree copies a package directory into dst (files + dirs, preserving the
// executable bit).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("plugin install: %s is not a regular file", path)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode()&0o777)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
