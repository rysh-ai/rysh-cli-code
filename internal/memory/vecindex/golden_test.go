// SPDX-License-Identifier: Apache-2.0

package vecindex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// goldenCase is one query in testdata/golden.json. Want lists the expected
// hits, best first, as "source#heading". The expectations were written by
// hand from the corpus before the index existed; they are not captured output.
type goldenCase struct {
	Query    string   `json:"query"`
	K        int      `json:"k"`
	MinScore float32  `json:"min_score"`
	Want     []string `json:"want"`
}

// indexCorpus chunks and embeds every file in testdata/corpus.
func indexCorpus(t *testing.T, emb Embedder) *Flat {
	t.Helper()
	f := newIndex(t, emb.Dim())
	paths, err := filepath.Glob(filepath.Join("testdata", "corpus", "*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d files)", err, len(paths))
	}
	sort.Strings(paths)
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		src := filepath.Base(p)
		chunks := ChunkMarkdown(src, string(raw), DefaultMaxChunk)
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Text
		}
		vecs, err := emb.Embed(context.Background(), texts)
		if err != nil {
			t.Fatal(err)
		}
		mustUpsert(t, f, src, chunks, vecs)
	}
	return f
}

func TestGoldenTopK(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	emb := fakeEmbedder{dim: 512}
	f := indexCorpus(t, emb)
	if f.Len() != 9 {
		t.Errorf("corpus indexed into %d chunks, want 9 (4 gateway.md, 2 strict.md, 3 kitchen.md)", f.Len())
	}
	for _, c := range cases {
		q, _ := emb.Embed(context.Background(), []string{c.Query})
		hits, err := f.Search(q[0], c.K, c.MinScore)
		if err != nil {
			t.Errorf("%q: %v", c.Query, err)
			continue
		}
		got := make([]string, len(hits))
		for i, h := range hits {
			got[i] = h.Source + "#" + h.Heading
			t.Logf("%q k=%d min=%v: #%d %s %.4f", c.Query, c.K, c.MinScore, i+1, got[i], h.Score)
		}
		if !reflect.DeepEqual(got, c.Want) {
			scores := make([]float32, len(hits))
			for i, h := range hits {
				scores[i] = h.Score
			}
			t.Errorf("%q k=%d min=%v:\n got  %q %v\n want %q", c.Query, c.K, c.MinScore, got, scores, c.Want)
		}
	}
}

// TestGoldenIndexingIsIdempotent re-indexes the unchanged corpus: nothing is
// added or removed.
func TestGoldenIndexingIsIdempotent(t *testing.T) {
	emb := fakeEmbedder{dim: 512}
	f := indexCorpus(t, emb)
	raw, err := os.ReadFile(filepath.Join("testdata", "corpus", "strict.md"))
	if err != nil {
		t.Fatal(err)
	}
	chunks := ChunkMarkdown("strict.md", string(raw), DefaultMaxChunk)
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	vecs, _ := emb.Embed(context.Background(), texts)
	st := mustUpsert(t, f, "strict.md", chunks, vecs)
	if st.Added != 0 || st.Removed != 0 || st.Unchanged != len(chunks) || len(chunks) == 0 {
		t.Fatalf("re-index of unchanged strict.md: %+v over %d chunks", st, len(chunks))
	}
}
