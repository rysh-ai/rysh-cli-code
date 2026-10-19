// SPDX-License-Identifier: Apache-2.0

package vecindex

import (
	"fmt"
	"math"
	"sort"
	"sync"
)

// Flat is the exact, brute-force cosine index. Vectors are normalised once on
// the way in, so a search is one dot product per stored chunk over a single
// contiguous []float32. It is safe for concurrent use: searches share a read
// lock, and an Upsert or Delete applies under the write lock all at once, so a
// reader sees a source either before or after a change, never half of it.
type Flat struct {
	dim int

	mu   sync.RWMutex
	vecs []float32 // len(meta)*dim, unit vectors, slot i at [i*dim:(i+1)*dim]
	meta []entry
	// slots maps source -> chunk ID -> slot in vecs/meta.
	slots map[string]map[string]int
}

type entry struct {
	source, id, heading, text string
}

var _ Index = (*Flat)(nil)

// New returns an empty Flat index for vectors of dimension dim.
func New(dim int) (*Flat, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("%w: dimension %d", ErrDimension, dim)
	}
	return &Flat{dim: dim, slots: map[string]map[string]int{}}, nil
}

// Dim is the vector dimension the index was built for.
func (f *Flat) Dim() int { return f.dim }

// Upsert implements Index. All input is checked before the lock is taken, so a
// rejected call changes nothing.
func (f *Flat) Upsert(source string, chunks []Chunk, vecs [][]float32) (UpsertStats, error) {
	if source == "" {
		return UpsertStats{}, fmt.Errorf("%w: empty source", ErrBadInput)
	}
	if len(chunks) != len(vecs) {
		return UpsertStats{}, fmt.Errorf("%w: %d chunks but %d vectors", ErrBadInput, len(chunks), len(vecs))
	}
	type incoming struct {
		chunk Chunk
		unit  []float32
	}
	want := make(map[string]incoming, len(chunks))
	order := make([]string, 0, len(chunks)) // first-seen order, so slots are deterministic
	for i, c := range chunks {
		if id := ChunkID(source, c.Text); c.ID != id {
			return UpsertStats{}, fmt.Errorf("%w: chunk %d has ID %q, want ChunkID(%q, text) = %q", ErrBadInput, i, c.ID, source, id)
		}
		unit, err := f.unit(vecs[i])
		if err != nil {
			return UpsertStats{}, fmt.Errorf("chunk %d: %w", i, err)
		}
		if _, dup := want[c.ID]; dup {
			continue // the same text twice in one source is one chunk
		}
		want[c.ID] = incoming{c, unit}
		order = append(order, c.ID)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var st UpsertStats
	have := f.slots[source]
	var stale []string
	for id := range have {
		if _, ok := want[id]; ok {
			st.Unchanged++
		} else {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	for _, id := range stale {
		f.remove(source, id)
		st.Removed++
	}
	for _, id := range order {
		if _, ok := f.slots[source][id]; ok {
			continue // unchanged content keeps its stored vector
		}
		in := want[id]
		if f.slots[source] == nil {
			f.slots[source] = map[string]int{}
		}
		f.slots[source][id] = len(f.meta)
		f.meta = append(f.meta, entry{source: source, id: id, heading: in.chunk.Heading, text: in.chunk.Text})
		f.vecs = append(f.vecs, in.unit...)
		st.Added++
	}
	if len(f.slots[source]) == 0 {
		delete(f.slots, source)
	}
	return st, nil
}

// Delete implements Index.
func (f *Flat) Delete(source string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.slots[source]))
	for id := range f.slots[source] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f.remove(source, id)
	}
	delete(f.slots, source)
	return len(ids)
}

// remove deletes one chunk by moving the last slot into its place. Callers
// hold the write lock. Slot order is not observable: Search orders by score,
// then source, then chunk ID.
func (f *Flat) remove(source, id string) {
	i, ok := f.slots[source][id]
	if !ok {
		return
	}
	delete(f.slots[source], id)
	last := len(f.meta) - 1
	if i != last {
		f.meta[i] = f.meta[last]
		copy(f.vecs[i*f.dim:(i+1)*f.dim], f.vecs[last*f.dim:])
		f.slots[f.meta[i].source][f.meta[i].id] = i
	}
	f.meta[last] = entry{}
	f.meta = f.meta[:last]
	f.vecs = f.vecs[:last*f.dim]
}

// Len implements Index.
func (f *Flat) Len() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.meta)
}

// Search implements Index.
func (f *Flat) Search(q []float32, k int, minScore float32) ([]Hit, error) {
	if k <= 0 {
		return nil, fmt.Errorf("%w: k = %d", ErrBadK, k)
	}
	if !finite(minScore) {
		return nil, fmt.Errorf("%w: minScore %v", ErrNotFinite, minScore)
	}
	uq, err := f.unit(q)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	f.mu.RLock()
	defer f.mu.RUnlock()
	n := len(f.meta)
	top := topK{f: f, k: k, c: make([]cand, 0, min(k, n))}
	for i := 0; i < n; i++ {
		s := dot(uq, f.vecs[i*f.dim:(i+1)*f.dim])
		if s > 1 { // rounding on unit vectors; keep scores in [-1, 1]
			s = 1
		} else if s < -1 {
			s = -1
		}
		if s >= minScore {
			top.offer(cand{s, i})
		}
	}
	sort.Slice(top.c, func(a, b int) bool { return top.better(top.c[a], top.c[b]) })
	hits := make([]Hit, len(top.c))
	for i, c := range top.c {
		e := f.meta[c.slot]
		hits[i] = Hit{Source: e.source, ChunkID: e.id, Heading: e.heading, Text: e.text, Score: c.score}
	}
	return hits, nil
}

// unit checks v and returns a normalised copy.
func (f *Flat) unit(v []float32) ([]float32, error) {
	if len(v) != f.dim {
		return nil, fmt.Errorf("%w: got %d, index is %d", ErrDimension, len(v), f.dim)
	}
	var sum float64
	for _, x := range v {
		if !finite(x) {
			return nil, ErrNotFinite
		}
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return nil, ErrZeroVector
	}
	norm := math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out, nil
}

func finite(x float32) bool {
	return !math.IsNaN(float64(x)) && !math.IsInf(float64(x), 0)
}

// dot is the inner product of two equal-length vectors, unrolled by four.
func dot(a, b []float32) float32 {
	b = b[:len(a)]
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3)
}

type cand struct {
	score float32
	slot  int
}

// topK keeps the best k candidates in a min-heap whose root is the worst kept
// one, so each stored chunk costs one comparison unless it makes the cut.
type topK struct {
	f *Flat
	k int
	c []cand
}

// better is the result order: higher score, then source, then chunk ID.
func (t *topK) better(a, b cand) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	ea, eb := &t.f.meta[a.slot], &t.f.meta[b.slot]
	if ea.source != eb.source {
		return ea.source < eb.source
	}
	return ea.id < eb.id
}

func (t *topK) offer(x cand) {
	if len(t.c) < t.k {
		t.c = append(t.c, x)
		for i := len(t.c) - 1; i > 0; { // sift up
			p := (i - 1) / 2
			if !t.better(t.c[p], t.c[i]) {
				break
			}
			t.c[p], t.c[i] = t.c[i], t.c[p]
			i = p
		}
		return
	}
	if !t.better(x, t.c[0]) {
		return
	}
	t.c[0] = x
	for i := 0; ; { // sift down: the root must be the worst
		l, r, w := 2*i+1, 2*i+2, i
		if l < len(t.c) && t.better(t.c[w], t.c[l]) {
			w = l
		}
		if r < len(t.c) && t.better(t.c[w], t.c[r]) {
			w = r
		}
		if w == i {
			return
		}
		t.c[i], t.c[w] = t.c[w], t.c[i]
		i = w
	}
}
