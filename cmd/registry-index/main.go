// registry-index — builds a publishable rysh package registry from a directory
// of packages (design 005).
//
// Given packages/<name>/{rysh.pkg.yaml,SKILL.md,...} it emits, into -out:
//
//	index.json                     the catalogue clients resolve @ns/name against
//	pkg/<name>-<version>.tar.gz    one artifact per package
//
// Publishing is then a file upload — no service. That is the "format open,
// service private" half of design 005: anyone can host this output, including
// a company that wants an internal-only registry.
//
// The entry checksum inside each manifest is COMPUTED here rather than
// hand-maintained. A checksum a human has to remember to update is a checksum
// that silently goes stale, and the install-time gate that compares against it
// would then either fail for everyone or be routinely --force'd past.
//
//	go run ./cmd/registry-index -src packages -out .build/registry
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/registry"
)

func main() {
	src := flag.String("src", "packages", "directory of package directories")
	out := flag.String("out", ".build/registry", "output directory for index.json and pkg/")
	baseURL := flag.String("base-url", "", "absolute prefix for artifact URLs (default: relative, so the registry can be relocated)")
	flag.Parse()

	if err := build(*src, *out, *baseURL); err != nil {
		fmt.Fprintf(os.Stderr, "registry-index: %v\n", err)
		os.Exit(1)
	}
}

func build(src, out, baseURL string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	pkgDir := filepath.Join(out, "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return err
	}

	idx := registry.Index{
		SchemaVersion: registry.IndexSchemaVersion,
		Generated:     time.Now().UTC().Format(time.RFC3339),
		Packages:      map[string]registry.IndexPackage{},
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("no package directories under %s", src)
	}

	for _, name := range names {
		dir := filepath.Join(src, name)
		m, tarball, err := packOne(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		artifact := fmt.Sprintf("%s-%s.tar.gz", registry.InstallName(m.Name), m.Version)
		if err := os.WriteFile(filepath.Join(pkgDir, artifact), tarball, 0o644); err != nil {
			return err
		}

		url := "pkg/" + artifact
		if baseURL != "" {
			url = strings.TrimSuffix(baseURL, "/") + "/pkg/" + artifact
		}

		p, ok := idx.Packages[m.Name]
		if !ok {
			p = registry.IndexPackage{Type: m.Type, Description: m.Description,
				Versions: map[string]registry.IndexVersion{}}
		}
		p.Versions[m.Version] = registry.IndexVersion{
			URL:      url,
			Checksum: registry.Sha256Hex(tarball),
		}
		idx.Packages[m.Name] = p

		fmt.Printf("  %-28s %-8s %s\n", m.Name, m.Version, artifact)
	}

	body, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	indexPath := filepath.Join(out, "index.json")
	if err := os.WriteFile(indexPath, body, 0o644); err != nil {
		return err
	}

	// The browsable landing page, generated next to the catalogue so the
	// published registry has a face without any service (design 005, B9).
	page, err := registry.RenderIndexHTML(&idx)
	if err != nil {
		return err
	}
	htmlPath := filepath.Join(out, "index.html")
	if err := os.WriteFile(htmlPath, page, 0o644); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s + index.html (%d packages)\n", indexPath, len(idx.Packages))
	return nil
}

// packOne reads a package directory, fills in the entry checksum, and returns
// the manifest plus a gzipped tar of the package rooted at "pkg/".
func packOne(dir string) (*registry.Manifest, []byte, error) {
	manifestPath := filepath.Join(dir, registry.ManifestFile)
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", registry.ManifestFile, err)
	}
	m, err := registry.ParseManifest(manifestRaw)
	if err != nil {
		return nil, nil, err
	}

	entryData, err := os.ReadFile(filepath.Join(dir, m.Entry))
	if err != nil {
		return nil, nil, fmt.Errorf("read entry %s: %w", m.Entry, err)
	}
	sum := registry.Sha256Hex(entryData)

	// Stamp the computed checksum into the shipped manifest. Authors leave the
	// field out; the builder is the single place it is derived, so it cannot
	// drift from the file it describes.
	shipped := stampChecksum(string(manifestRaw), sum)
	m.Checksum = sum

	files, err := collect(dir)
	if err != nil {
		return nil, nil, err
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		body := f.data
		if f.rel == registry.ManifestFile {
			body = []byte(shipped)
		}
		hdr := &tar.Header{
			Name: "pkg/" + f.rel,
			Mode: 0o644,
			Size: int64(len(body)),
			// A fixed mtime keeps the tarball byte-identical across builds, so
			// an unchanged package does not churn its checksum in the index.
			ModTime: time.Unix(0, 0).UTC(),
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, nil, err
	}
	return m, buf.Bytes(), nil
}

type pkgFile struct {
	rel  string
	data []byte
}

func collect(dir string) ([]pkgFile, error) {
	var out []pkgFile
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, ".") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, pkgFile{rel: filepath.ToSlash(rel), data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// stampChecksum replaces or appends the manifest's checksum line.
func stampChecksum(manifest, sum string) string {
	lines := strings.Split(manifest, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "checksum:") {
			lines[i] = "checksum: " + sum
			return strings.Join(lines, "\n")
		}
	}
	trimmed := strings.TrimRight(manifest, "\n")
	return trimmed + "\nchecksum: " + sum + "\n"
}
