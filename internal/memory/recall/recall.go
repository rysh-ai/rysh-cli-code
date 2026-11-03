// SPDX-License-Identifier: Apache-2.0

// Package recall is semantic memory's search (design 010 §3, slice S4): it
// keeps the vector index (vecindex, saved by S2) in step with the memory
// sources and answers cited top-k queries through an embedder (S3).
//
// Indexing is on demand: every Search first syncs the index with the sources
// it is handed. Unchanged chunks keep their stored vectors and cost no
// embedding call, so a sync is a re-read and a hash per chunk; only new or
// changed text is embedded. Sources that disappeared are dropped. That keeps
// results exactly as fresh as the sources with no background goroutine and no
// new bus subjects.
package recall

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rysh-ai/rysh-cli-code/internal/memory/vecindex"
)

// MaxK caps the hits one search returns.
const MaxK = 20

// Embedder is a vecindex.Embedder that names its model, so the saved index is
// rebuilt when the model or dimension changes.
type Embedder interface {
	vecindex.Embedder
	ID() string
}

// Source is one document to index. Name is what a hit cites.
type Source struct{ Name, Text string }

// Indexer owns one index file. It is safe for concurrent use; searches are
// serialised (each one may write the file).
type Indexer struct {
	mu   sync.Mutex
	emb  Embedder
	path string
	idx  *vecindex.Flat
}

// Open loads the index at path for emb. A missing, stale or unreadable file
// starts empty (vecindex.Load's ErrReindex) and is rebuilt by the first search.
func Open(path string, emb Embedder) *Indexer {
	idx, err := vecindex.Load(path, emb.Dim(), emb.ID())
	if err != nil && !errors.Is(err, vecindex.ErrReindex) {
		idx, _ = vecindex.New(emb.Dim())
	}
	return &Indexer{emb: emb, path: path, idx: idx}
}

// Search syncs the index with srcs, then returns at most k (1..MaxK, larger is
// clamped) hits for query, best first, each naming its source and heading.
func (x *Indexer) Search(ctx context.Context, srcs []Source, query string, k int) ([]vecindex.Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("recall: empty query")
	}
	if k <= 0 {
		return nil, fmt.Errorf("recall: k must be 1..%d", MaxK)
	}
	k = min(k, MaxK)
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(ctx, srcs); err != nil {
		return nil, err
	}
	q, err := x.emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("recall: embed query: %w", err)
	}
	if len(q) != 1 {
		return nil, errors.New("recall: embedder returned no query vector")
	}
	return x.idx.Search(q[0], k, 0)
}

// sync makes the index hold exactly srcs, embedding only new chunk text, and
// saves the file when anything changed. Callers hold x.mu.
func (x *Indexer) sync(ctx context.Context, srcs []Source) error {
	changed := false
	keep := make(map[string]bool, len(srcs))
	for _, s := range srcs {
		if s.Name == "" {
			continue
		}
		keep[s.Name] = true
		chunks := vecindex.ChunkMarkdown(s.Name, s.Text, vecindex.DefaultMaxChunk)
		vecs := make([][]float32, len(chunks))
		var missing []int
		for i, c := range chunks {
			if v, ok := x.idx.Vector(s.Name, c.ID); ok {
				vecs[i] = v
			} else {
				missing = append(missing, i)
			}
		}
		if len(missing) > 0 {
			texts := make([]string, len(missing))
			for j, i := range missing {
				texts[j] = chunks[i].Text
			}
			got, err := x.emb.Embed(ctx, texts)
			if err != nil {
				return fmt.Errorf("recall: embed %s: %w", s.Name, err)
			}
			if len(got) != len(missing) {
				return fmt.Errorf("recall: embedder returned %d vectors for %d texts", len(got), len(missing))
			}
			for j, i := range missing {
				vecs[i] = got[j]
			}
		}
		st, err := x.idx.Upsert(s.Name, chunks, vecs)
		if err != nil {
			return fmt.Errorf("recall: index %s: %w", s.Name, err)
		}
		if st.Added > 0 || st.Removed > 0 {
			changed = true
		}
	}
	for _, name := range x.idx.Sources() {
		if !keep[name] && x.idx.Delete(name) > 0 {
			changed = true
		}
	}
	if changed {
		if err := x.idx.Save(x.path, x.emb.ID()); err != nil {
			return fmt.Errorf("recall: save index: %w", err)
		}
	}
	return nil
}
