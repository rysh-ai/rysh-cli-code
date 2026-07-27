package registry

// Client-side search and update over the static index (design 005, B9).
//
// The index deliberately stays a static JSON file — but nothing about search
// requires a service: the whole catalogue is already in the client's hands
// after one fetch, so `rysh search` is a substring scan over it, and
// `rysh update` is a comparison between the lockfile and the index. Both stay
// pure here so they are testable without HTTP.

import (
	"sort"
	"strings"
)

// SearchResult is one `rysh search` hit.
type SearchResult struct {
	Name        string
	Latest      string
	Type        string
	Description string
}

// Search returns the packages whose name or description contains query
// (case-insensitive; empty query matches everything), sorted by name, each
// with its highest published version.
func (idx *Index) Search(query string) []SearchResult {
	q := strings.ToLower(strings.TrimSpace(query))
	var out []SearchResult
	for name, p := range idx.Packages {
		if q != "" &&
			!strings.Contains(strings.ToLower(name), q) &&
			!strings.Contains(strings.ToLower(p.Description), q) {
			continue
		}
		out = append(out, SearchResult{
			Name:        name,
			Latest:      highestVersion(p.Versions),
			Type:        p.Type,
			Description: p.Description,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// UpdateCandidate is one installed package with a newer published version.
type UpdateCandidate struct {
	Name      string
	Installed string
	Latest    string
	Artifact  IndexVersion
}

// Outdated compares the lockfile against the index and returns the installed
// packages the index publishes a strictly newer version of, sorted by name.
// Only namespaced installs participate: a package installed from a local dir
// or ad-hoc tarball has no index identity to update from.
func (idx *Index) Outdated(l *Lock) []UpdateCandidate {
	var out []UpdateCandidate
	for name, e := range l.Packages {
		if !IsNamespaced(name) {
			continue
		}
		p, ok := idx.Packages[name]
		if !ok {
			continue
		}
		latest := highestVersion(p.Versions)
		if latest == "" || compareSemver(latest, e.Version) <= 0 {
			continue
		}
		out = append(out, UpdateCandidate{
			Name:      name,
			Installed: e.Version,
			Latest:    latest,
			Artifact:  p.Versions[latest],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
