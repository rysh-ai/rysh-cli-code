// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"strings"
	"testing"
)

func searchTestIndex() *Index {
	return &Index{
		SchemaVersion: IndexSchemaVersion,
		Generated:     "2026-07-23T00:00:00Z",
		Packages: map[string]IndexPackage{
			"@rysh/code-reviewer": {
				Type: "agent", Description: "Reviews diffs for defects",
				Versions: map[string]IndexVersion{
					"0.1.0": {URL: "pkg/cr-0.1.0.tar.gz", Checksum: "sha256:aa"},
					"0.2.0": {URL: "pkg/cr-0.2.0.tar.gz", Checksum: "sha256:bb"},
				},
			},
			"@rysh/pr-writer": {
				Type: "agent", Description: "Writes pull-request bodies",
				Versions: map[string]IndexVersion{
					"1.0.0": {URL: "pkg/pw-1.0.0.tar.gz", Checksum: "sha256:cc"},
				},
			},
			"@rysh/test-writer": {
				Type: "agent", Description: "Drafts missing unit tests",
				Versions: map[string]IndexVersion{
					"0.9.0": {URL: "pkg/tw-0.9.0.tar.gz", Checksum: "sha256:dd"},
				},
			},
		},
	}
}

// TestSearchMatchesNameAndDescription: `rysh search` is a case-insensitive
// substring scan over name + description, sorted by name, latest-version
// resolved numerically (0.10 > 0.9 class of bug is compareSemver's job).
func TestSearchMatchesNameAndDescription(t *testing.T) {
	idx := searchTestIndex()

	byName := idx.Search("REVIEW")
	if len(byName) != 1 || byName[0].Name != "@rysh/code-reviewer" || byName[0].Latest != "0.2.0" {
		t.Fatalf("search by name = %+v", byName)
	}

	byDesc := idx.Search("pull-request")
	if len(byDesc) != 1 || byDesc[0].Name != "@rysh/pr-writer" {
		t.Fatalf("search by description = %+v", byDesc)
	}

	all := idx.Search("")
	if len(all) != 3 || all[0].Name != "@rysh/code-reviewer" || all[2].Name != "@rysh/test-writer" {
		t.Fatalf("empty query must return everything sorted by name: %+v", all)
	}

	if got := idx.Search("no-such-thing"); len(got) != 0 {
		t.Fatalf("non-matching query returned %+v", got)
	}
}

// TestOutdated: only namespaced installs with a strictly newer index version
// are update candidates; up-to-date, unknown-to-index, and path-installed
// packages are skipped.
func TestOutdated(t *testing.T) {
	idx := searchTestIndex()
	lock := &Lock{Packages: map[string]LockEntry{
		"@rysh/code-reviewer": {Version: "0.1.0", Type: "agent"}, // 0.2.0 available
		"@rysh/pr-writer":     {Version: "1.0.0", Type: "agent"}, // current
		"local-thing":         {Version: "0.0.1", Type: "agent"}, // not namespaced
		"@rysh/vanished":      {Version: "0.1.0", Type: "agent"}, // not in index
	}}

	out := idx.Outdated(lock)
	if len(out) != 1 {
		t.Fatalf("outdated = %+v, want exactly the code-reviewer", out)
	}
	c := out[0]
	if c.Name != "@rysh/code-reviewer" || c.Installed != "0.1.0" || c.Latest != "0.2.0" {
		t.Fatalf("candidate = %+v", c)
	}
	if c.Artifact.Checksum != "sha256:bb" {
		t.Fatalf("candidate must carry the LATEST artifact, got %+v", c.Artifact)
	}
}

// TestRenderIndexHTML: the landing page lists every package with its latest
// version and escapes package-supplied content.
func TestRenderIndexHTML(t *testing.T) {
	idx := searchTestIndex()
	idx.Packages["@evil/xss"] = IndexPackage{
		Type: "agent", Description: `<script>alert(1)</script>`,
		Versions: map[string]IndexVersion{"0.0.1": {URL: "pkg/x.tar.gz", Checksum: "sha256:ee"}},
	}

	page, err := RenderIndexHTML(idx)
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)
	for _, want := range []string{"@rysh/code-reviewer", "0.2.0", "Reviews diffs for defects", "index.json"} {
		if !strings.Contains(s, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	if strings.Contains(s, "<script>alert(1)</script>") {
		t.Fatal("package-supplied description rendered unescaped")
	}
	if !strings.Contains(s, "&lt;script&gt;") {
		t.Fatal("escaped description not present — content silently dropped?")
	}
}
