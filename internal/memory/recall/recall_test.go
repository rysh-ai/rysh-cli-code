// SPDX-License-Identifier: Apache-2.0

package recall

import (
	"context"
	"hash/fnv"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// countingEmbedder is a deterministic bag-of-words embedder that counts the
// texts it is asked to embed. No network, no model.
type countingEmbedder struct {
	dim   int
	id    string
	calls int
}

func (e *countingEmbedder) Dim() int   { return e.dim }
func (e *countingEmbedder) ID() string { return e.id }
func (e *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls += len(texts)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, e.dim)
		v[0] = 0.01 // never the zero vector
		for _, w := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			h := fnv.New32a()
			h.Write([]byte(w))
			v[1+int(h.Sum32()%uint32(e.dim-1))]++
		}
		out[i] = v
	}
	return out, nil
}

func sources() []Source {
	return []Source{
		{Name: "memory:build", Text: "Build with make test before every commit."},
		{Name: "memory:tabs", Text: "The user prefers tabs over spaces in Go files."},
		{Name: "RYSH.md", Text: "# Deploy\n\nDeploy with the rollback script, never by hand.\n"},
	}
}

func TestSearchCitesTheRightSource(t *testing.T) {
	emb := &countingEmbedder{dim: 64, id: "fake:64"}
	x := Open(filepath.Join(t.TempDir(), "memory", "vecindex.json"), emb)
	hits, err := x.Search(context.Background(), sources(), "how do we deploy and roll back", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Source != "RYSH.md" || hits[0].Heading != "Deploy" {
		t.Fatalf("top hit = %+v, want RYSH.md#Deploy", hits)
	}
	if len(hits) > 2 {
		t.Errorf("k=2 returned %d hits", len(hits))
	}
}

// TestUnchangedTextIsNotEmbeddedAgain: the second search embeds only the
// query; a changed source embeds only its new chunk; a vanished source is
// dropped; and the index survives a reopen.
func TestUnchangedTextIsNotEmbeddedAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory", "vecindex.json")
	emb := &countingEmbedder{dim: 64, id: "fake:64"}
	x := Open(path, emb)
	ctx := context.Background()
	if _, err := x.Search(ctx, sources(), "tabs", 3); err != nil {
		t.Fatal(err)
	}
	first := emb.calls
	if first != 4 { // 3 chunks + the query
		t.Fatalf("first search embedded %d texts, want 4", first)
	}
	if _, err := x.Search(ctx, sources(), "tabs", 3); err != nil {
		t.Fatal(err)
	}
	if got := emb.calls - first; got != 1 {
		t.Errorf("unchanged search embedded %d texts, want 1 (the query)", got)
	}

	src := sources()[:2] // RYSH.md is gone
	src[0].Text = "Build with go test ./... before every commit."
	before := emb.calls
	hits, err := x.Search(ctx, src, "deploy rollback", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := emb.calls - before; got != 2 { // the changed entry + the query
		t.Errorf("changed search embedded %d texts, want 2", got)
	}
	for _, h := range hits {
		if h.Source == "RYSH.md" {
			t.Errorf("vanished source still answers")
		}
	}

	// Reopen with the same embedder: nothing to re-embed but the query.
	emb2 := &countingEmbedder{dim: 64, id: "fake:64"}
	y := Open(path, emb2)
	if _, err := y.Search(ctx, src, "tabs", 3); err != nil {
		t.Fatal(err)
	}
	if emb2.calls != 1 {
		t.Errorf("after reopen embedded %d texts, want 1 (the query)", emb2.calls)
	}

	// Another embedder ID: the file is rebuilt, never mixed.
	emb3 := &countingEmbedder{dim: 64, id: "other:64"}
	z := Open(path, emb3)
	if _, err := z.Search(ctx, src, "tabs", 3); err != nil {
		t.Fatal(err)
	}
	if emb3.calls != 3 { // 2 chunks + the query
		t.Errorf("embedder change embedded %d texts, want 3 (full rebuild)", emb3.calls)
	}
}

func TestSearchBoundsK(t *testing.T) {
	x := Open(filepath.Join(t.TempDir(), "v.json"), &countingEmbedder{dim: 16, id: "f"})
	if _, err := x.Search(context.Background(), sources(), "x", 0); err == nil {
		t.Errorf("k=0 accepted")
	}
	if _, err := x.Search(context.Background(), sources(), "  ", 3); err == nil {
		t.Errorf("empty query accepted")
	}
	hits, err := x.Search(context.Background(), sources(), "tabs", 1000)
	if err != nil || len(hits) > MaxK {
		t.Errorf("k=1000: %d hits, err %v; want at most MaxK", len(hits), err)
	}
}
