// SPDX-License-Identifier: Apache-2.0

package registry

// Namespaced package resolution — the `@ns/name` half of design 005.
//
// The client could already install from a directory, a tarball or a URL. What
// it could not do is the thing the docs advertise: `rysh install
// @rysh/code-reviewer`. That needs a name→artifact mapping, which is what this
// file adds.
//
// THE INDEX IS A STATIC FILE, ON PURPOSE.
//
// Design 005 states the model as "format open, service private — the npm
// model". A JSON document served over plain HTTP is the smallest thing that
// satisfies the open half: anyone can host a mirror, a company can point
// `registry.url` at an internal bucket, and there is no service to run, scale
// or authenticate against for the read path. Publishing is a file upload.
//
// What this deliberately does NOT do:
//   - no search (that is a service concern, not a resolution concern);
//   - no dependency resolution — packages are flat, as design 005 requires;
//   - no signature *enforcement*. Signatures are carried and surfaced by the
//     existing consent gate; making the index the trust root would be worse
//     than the status quo, because a compromised index could then vouch for
//     anything. Trust stays with the checksum in the index plus the manifest
//     gate at install time.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultIndexURL is where the first-party index lives. Overridable via
// config (`registry.url`) or RYSH_REGISTRY_URL so a company can host its own.
const DefaultIndexURL = "https://packages.rysh.ai/registry/index.json"

// Index is the published catalogue. Versions are keyed by exact semver string;
// resolution picks the highest by numeric comparison, so "0.10.0" > "0.9.0"
// rather than sorting lexically.
type Index struct {
	// SchemaVersion lets a future format change be rejected loudly by an old
	// client instead of being silently mis-parsed.
	SchemaVersion int                     `json:"schema_version"`
	Generated     string                  `json:"generated,omitempty"`
	Packages      map[string]IndexPackage `json:"packages"`
}

// IndexPackage is one named package and its published versions.
type IndexPackage struct {
	Description string                  `json:"description,omitempty"`
	Type        string                  `json:"type,omitempty"` // agent|loop|recipe|humanoid
	Versions    map[string]IndexVersion `json:"versions"`
}

// IndexVersion points at one artifact.
type IndexVersion struct {
	// URL is the tarball. Relative values resolve against the index URL, so a
	// mirror can be moved wholesale without rewriting every entry.
	URL string `json:"url"`
	// Checksum is sha256:… over the tarball bytes. Verified before the archive
	// is opened — see ResolveSpec's caller.
	Checksum string `json:"checksum"`
}

// IndexSchemaVersion is the format this client speaks.
const IndexSchemaVersion = 1

// IsNamespaced reports whether spec looks like `@ns/name[@version]` rather than
// a path or URL.
func IsNamespaced(spec string) bool {
	return strings.HasPrefix(spec, "@") && strings.Contains(spec, "/")
}

// SplitSpec parses `@ns/name` or `@ns/name@version`.
// The version separator is the LAST '@', since the name itself starts with one.
func SplitSpec(spec string) (name, version string, err error) {
	if !IsNamespaced(spec) {
		return "", "", fmt.Errorf("not a namespaced package: %q", spec)
	}
	name = spec
	if i := strings.LastIndex(spec, "@"); i > 0 { // i>0 skips the leading '@'
		name, version = spec[:i], spec[i+1:]
		if version == "" {
			return "", "", fmt.Errorf("empty version in %q", spec)
		}
	}
	if strings.Count(name, "/") != 1 || strings.HasSuffix(name, "/") {
		return "", "", fmt.Errorf("malformed package name %q (want @namespace/name)", name)
	}
	return name, version, nil
}

// FetchIndex retrieves and validates the index document.
func FetchIndex(indexURL string, timeout time.Duration) (*Index, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	var data []byte

	// A local path is accepted so an air-gapped install (and the tests) can
	// point at a file without standing up a web server.
	if !strings.HasPrefix(indexURL, "http://") && !strings.HasPrefix(indexURL, "https://") {
		b, err := os.ReadFile(indexURL)
		if err != nil {
			return nil, fmt.Errorf("read index %s: %w", indexURL, err)
		}
		data = b
	} else {
		client := &http.Client{Timeout: timeout}
		resp, err := client.Get(indexURL)
		if err != nil {
			return nil, fmt.Errorf("fetch index %s: %w", indexURL, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch index %s: HTTP %d", indexURL, resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB ceiling
		if err != nil {
			return nil, fmt.Errorf("read index %s: %w", indexURL, err)
		}
		data = b
	}

	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse index %s: %w", indexURL, err)
	}
	if idx.SchemaVersion != IndexSchemaVersion {
		return nil, fmt.Errorf("index %s has schema_version %d, this rysh speaks %d — upgrade rysh",
			indexURL, idx.SchemaVersion, IndexSchemaVersion)
	}
	if len(idx.Packages) == 0 {
		return nil, fmt.Errorf("index %s lists no packages", indexURL)
	}
	return &idx, nil
}

// Resolve maps `@ns/name[@version]` to a concrete artifact. An empty version
// selects the highest published one.
func (idx *Index) Resolve(spec string) (name, version string, v IndexVersion, err error) {
	name, version, err = SplitSpec(spec)
	if err != nil {
		return "", "", IndexVersion{}, err
	}
	pkg, ok := idx.Packages[name]
	if !ok {
		return "", "", IndexVersion{}, fmt.Errorf("package %s not found in the registry%s", name, didYouMean(idx, name))
	}
	if len(pkg.Versions) == 0 {
		return "", "", IndexVersion{}, fmt.Errorf("package %s has no published versions", name)
	}

	if version == "" {
		version = highestVersion(pkg.Versions)
	}
	got, ok := pkg.Versions[version]
	if !ok {
		return "", "", IndexVersion{}, fmt.Errorf("package %s has no version %s (published: %s)",
			name, version, strings.Join(sortedVersions(pkg.Versions), ", "))
	}
	if strings.TrimSpace(got.URL) == "" {
		return "", "", IndexVersion{}, fmt.Errorf("package %s@%s has no url in the index", name, version)
	}
	return name, version, got, nil
}

// didYouMean offers same-namespace neighbours, which is the common typo.
func didYouMean(idx *Index, name string) string {
	ns := name
	if i := strings.Index(name, "/"); i >= 0 {
		ns = name[:i]
	}
	var near []string
	for k := range idx.Packages {
		if strings.HasPrefix(k, ns+"/") {
			near = append(near, k)
		}
	}
	if len(near) == 0 {
		return ""
	}
	sort.Strings(near)
	if len(near) > 5 {
		near = near[:5]
	}
	return ". Available in " + ns + ": " + strings.Join(near, ", ")
}

func sortedVersions(m map[string]IndexVersion) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return compareSemver(out[i], out[j]) < 0 })
	return out
}

func highestVersion(m map[string]IndexVersion) string {
	best := ""
	for k := range m {
		if best == "" || compareSemver(k, best) > 0 {
			best = k
		}
	}
	return best
}

// compareSemver compares dotted numeric versions field by field. Non-numeric
// fields (pre-release tails like "1.0.0-rc1") fall back to string comparison on
// that field, which is enough to keep ordering stable without pulling in a
// semver dependency for what is a flat, first-party catalogue.
func compareSemver(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var af, bf string
		if i < len(as) {
			af = as[i]
		}
		if i < len(bs) {
			bf = bs[i]
		}
		ai, aerr := strconv.Atoi(af)
		bi, berr := strconv.Atoi(bf)
		if aerr == nil && berr == nil {
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
			continue
		}
		if af != bf {
			if af < bf {
				return -1
			}
			return 1
		}
	}
	return 0
}

// AbsoluteURL resolves a possibly-relative artifact URL against the index URL,
// so a whole registry can be relocated by moving the directory.
func AbsoluteURL(indexURL, artifactURL string) string {
	if strings.HasPrefix(artifactURL, "http://") || strings.HasPrefix(artifactURL, "https://") {
		return artifactURL
	}
	if i := strings.LastIndex(indexURL, "/"); i >= 0 {
		return indexURL[:i+1] + strings.TrimPrefix(artifactURL, "./")
	}
	return artifactURL
}

// InstallFromIndex downloads an indexed artifact, verifies its digest, and only
// then unpacks it.
//
// The ordering is the point. InstallFromTarball streams the response straight
// into extractTarGz, so by the time anything could be checked the archive has
// already been written to disk. For a registry install the index checksum is
// the only thing standing between "a name" and "code in ~/.rysh", so the bytes
// are buffered, hashed, compared, and discarded on mismatch — nothing is
// extracted from an artifact that failed verification.
//
// A missing checksum in the index is refused rather than warned about: an
// unverifiable artifact from a name-based install is exactly the case where a
// warning would be clicked through.
func InstallFromIndex(artifactURL, expectedChecksum, ryshDir string, opts Options) (*Manifest, []string, error) {
	if strings.TrimSpace(expectedChecksum) == "" {
		return nil, nil, fmt.Errorf("index entry for %s has no checksum — refusing to install unverifiable bytes", artifactURL)
	}

	var raw []byte
	if strings.HasPrefix(artifactURL, "http://") || strings.HasPrefix(artifactURL, "https://") {
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Get(artifactURL)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch %s: %w", artifactURL, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, nil, fmt.Errorf("fetch %s: HTTP %d", artifactURL, resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20)) // 64 MiB ceiling
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", artifactURL, err)
		}
		raw = b
	} else {
		b, err := os.ReadFile(artifactURL)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", artifactURL, err)
		}
		raw = b
	}

	// Sha256Hex already returns a "sha256:"-prefixed digest; prefixing again
	// here produced "sha256:sha256:…" on both sides of the comparison, which
	// matched itself and would have rejected every correctly-generated index.
	got := Sha256Hex(raw)
	want := expectedChecksum
	if !strings.Contains(want, ":") {
		want = "sha256:" + want
	}
	if !strings.EqualFold(got, want) {
		return nil, nil, fmt.Errorf(
			"checksum mismatch for %s\n  index: %s\n  actual: %s\nrefusing to install (nothing was extracted)",
			artifactURL, want, got)
	}

	tmp, err := os.MkdirTemp("", "rysh-pkg-*")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	root, err := extractTarGz(bytes.NewReader(raw), tmp)
	if err != nil {
		return nil, nil, err
	}
	return InstallFromDir(root, ryshDir, opts)
}
