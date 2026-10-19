// SPDX-License-Identifier: Apache-2.0

// Package vecindex is the vector index behind semantic memory (design 010).
//
// Founder ruling D-5 (2026-10-09): the index is pure Go. No cgo and no
// sqlite-vec, so the single static, cross-compiled binary is kept. It starts as
// exact (brute-force) cosine over float32 vectors. An approximate structure is
// added only when a measured corpus needs one, and it goes behind the same
// Index interface.
//
// S1 (E25 T6) is the in-memory index, the markdown chunker and the Embedder
// seam. Nothing calls it yet: persistence, a real embedder, the IndexerActor
// and `context action: search` are later slices.
package vecindex

import (
	"context"
	"errors"
)

// Chunk is one retrievable piece of a source document.
type Chunk struct {
	// ID is ChunkID(source, Text): a stable hash, so the same text from the
	// same source always gets the same ID, across runs and machines.
	ID string
	// Heading is the nearest markdown heading above the chunk, without the
	// leading #s. It is empty before the first heading. It is carried for
	// citation only and is not part of the ID.
	Heading string
	// Text is the chunk's content.
	Text string
}

// Hit is one search result. Every hit names its source, so recall is always
// citable.
type Hit struct {
	Source  string
	ChunkID string
	Heading string
	Text    string
	// Score is the cosine similarity of the query and the chunk, in [-1, 1].
	Score float32
}

// UpsertStats says what an Upsert changed. Re-indexing unchanged content
// reports only Unchanged.
type UpsertStats struct {
	Added     int
	Removed   int
	Unchanged int
}

// Index stores chunk vectors per source and answers nearest-neighbour queries.
// Implementations are safe for concurrent use.
type Index interface {
	// Upsert makes chunks the complete set of chunks for source. Chunks are
	// keyed by (source, chunk ID): a key already present keeps its stored
	// vector, keys no longer present are removed, and no other source is
	// touched. vecs[i] is the embedding of chunks[i]. On error the index is
	// unchanged.
	Upsert(source string, chunks []Chunk, vecs [][]float32) (UpsertStats, error)
	// Delete removes every chunk of source and returns how many it removed.
	Delete(source string) int
	// Search returns at most k hits with Score >= minScore, best first. Equal
	// scores are ordered by source, then chunk ID, so the order never depends
	// on insertion order.
	Search(q []float32, k int, minScore float32) ([]Hit, error)
	// Len is the number of chunks stored.
	Len() int
}

// Embedder turns texts into vectors of a fixed dimension. S1 has only a
// deterministic fake, in the tests; a real provider is S3.
type Embedder interface {
	Dim() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Errors returned for bad input. The index never panics on caller input.
var (
	// ErrDimension: a vector's length is not the index dimension, or the
	// dimension given to New is not positive.
	ErrDimension = errors.New("vecindex: vector dimension mismatch")
	// ErrZeroVector: a vector has zero norm, so its cosine is undefined.
	ErrZeroVector = errors.New("vecindex: zero vector")
	// ErrNotFinite: a vector component, or minScore, is NaN or infinite.
	ErrNotFinite = errors.New("vecindex: NaN or infinite value")
	// ErrBadK: k is not positive.
	ErrBadK = errors.New("vecindex: k must be positive")
	// ErrBadInput: an empty source, len(chunks) != len(vecs), or a chunk whose
	// ID is not ChunkID(source, Text).
	ErrBadInput = errors.New("vecindex: bad input")
)
