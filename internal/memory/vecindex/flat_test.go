// SPDX-License-Identifier: Apache-2.0

package vecindex

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
)

// newIndex is New for tests that pass a valid dimension.
func newIndex(t *testing.T, dim int) *Flat {
	t.Helper()
	f, err := New(dim)
	if err != nil {
		t.Fatalf("New(%d): %v", dim, err)
	}
	return f
}

// chunksOf makes chunks of source with the given texts, IDs filled in.
func chunksOf(source string, texts ...string) []Chunk {
	out := make([]Chunk, len(texts))
	for i, s := range texts {
		out[i] = Chunk{ID: ChunkID(source, s), Text: s}
	}
	return out
}

// noPanic runs f and turns a panic into a test failure, so a panicking input
// path is reported as an assertion, not as a crashed test binary.
func noPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked: %v", what, r)
		}
	}()
	f()
}

func mustUpsert(t *testing.T, f *Flat, source string, chunks []Chunk, vecs [][]float32) UpsertStats {
	t.Helper()
	st, err := f.Upsert(source, chunks, vecs)
	if err != nil {
		t.Fatalf("Upsert(%s): %v", source, err)
	}
	return st
}

func mustSearch(t *testing.T, f *Flat, q []float32, k int, min float32) []Hit {
	t.Helper()
	hits, err := f.Search(q, k, min)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return hits
}

func keys(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Source + "|" + h.Text
	}
	return out
}

func TestSearchRanksByCosine(t *testing.T) {
	f := newIndex(t, 2)
	mustUpsert(t, f, "s", chunksOf("s", "east", "north-east", "north", "west"),
		[][]float32{{1, 0}, {3, 3}, {0, 5}, {-2, 0}})
	hits := mustSearch(t, f, []float32{2, 0}, 10, -1)
	want := []string{"s|east", "s|north-east", "s|north", "s|west"}
	if got := keys(hits); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	wantScores := []float32{1, float32(1 / math.Sqrt2), 0, -1}
	for i, h := range hits {
		if math.Abs(float64(h.Score-wantScores[i])) > 1e-6 {
			t.Errorf("%s score = %v, want %v", h.Text, h.Score, wantScores[i])
		}
		if h.ChunkID != ChunkID("s", h.Text) {
			t.Errorf("%s ChunkID = %q", h.Text, h.ChunkID)
		}
	}
	if got := keys(mustSearch(t, f, []float32{2, 0}, 2, -1)); !reflect.DeepEqual(got, want[:2]) {
		t.Fatalf("k=2: %v, want %v", got, want[:2])
	}
}

func TestSearchHonoursTheScoreThreshold(t *testing.T) {
	f := newIndex(t, 2)
	mustUpsert(t, f, "s", chunksOf("s", "east", "north-east", "north", "west"),
		[][]float32{{1, 0}, {3, 3}, {0, 5}, {-2, 0}})
	q := []float32{1, 0}
	cases := []struct {
		min  float32
		want []string
	}{
		{0.5, []string{"s|east", "s|north-east"}},
		{0, []string{"s|east", "s|north-east", "s|north"}}, // a score equal to minScore is kept
		{1, []string{"s|east"}},
		{1.01, []string{}},
		{-1, []string{"s|east", "s|north-east", "s|north", "s|west"}},
	}
	for _, c := range cases {
		if got := keys(mustSearch(t, f, q, 10, c.min)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("minScore %v: %v, want %v", c.min, got, c.want)
		}
	}
}

func TestSearchBreaksTiesBySourceThenChunkID(t *testing.T) {
	// Insertion order is scrambled on purpose; the result order must not
	// depend on it.
	f := newIndex(t, 3)
	same := []float32{1, 1, 1} // cosine 0.816 with the query; "best" scores 0.943
	for _, src := range []string{"c.md", "a.md", "b.md"} {
		cs := chunksOf(src, "x", "y", "z")
		mustUpsert(t, f, src, cs, [][]float32{same, same, same})
	}
	mustUpsert(t, f, "0-best.md", chunksOf("0-best.md", "best"), [][]float32{{1, 1, 0.5}})
	hits := mustSearch(t, f, []float32{1, 1, 0}, 100, -1)
	if len(hits) != 10 || hits[0].Source != "0-best.md" {
		t.Fatalf("want the strictly better hit first, then 9 ties: %v", keys(hits))
	}
	for i := 2; i < len(hits); i++ {
		a, b := hits[i-1], hits[i]
		if a.Score != b.Score {
			t.Fatalf("hits %d and %d should tie: %v vs %v", i-1, i, a.Score, b.Score)
		}
		if a.Source > b.Source || (a.Source == b.Source && a.ChunkID >= b.ChunkID) {
			t.Fatalf("tie order broken at %d: (%s,%s) before (%s,%s)", i, a.Source, a.ChunkID, b.Source, b.ChunkID)
		}
	}
	// Cutting at k keeps the tie winners, not whichever came first.
	top := mustSearch(t, f, []float32{1, 1, 0}, 3, -1)
	if !reflect.DeepEqual(top, hits[:3]) {
		t.Fatalf("k=3 = %v, want %v", keys(top), keys(hits[:3]))
	}
	if top[1].Source != "a.md" || top[2].Source != "a.md" {
		t.Fatalf("k=3 tie winners are not from a.md: %v", keys(top))
	}
}

func TestUpsertIsIdempotent(t *testing.T) {
	f := newIndex(t, 2)
	cs := chunksOf("n.md", "one", "two", "three")
	vecs := [][]float32{{1, 0}, {0, 1}, {1, 1}}
	if st := mustUpsert(t, f, "n.md", cs, vecs); st != (UpsertStats{Added: 3}) {
		t.Fatalf("first upsert stats = %+v", st)
	}
	before := mustSearch(t, f, []float32{1, 0.2}, 10, -1)
	for i := 0; i < 3; i++ {
		if st := mustUpsert(t, f, "n.md", cs, vecs); st != (UpsertStats{Unchanged: 3}) {
			t.Fatalf("re-upsert %d stats = %+v, want only Unchanged", i, st)
		}
	}
	// Unchanged content keeps its stored vector: a no-op even if the caller
	// re-embedded it differently.
	if st := mustUpsert(t, f, "n.md", cs, [][]float32{{0, 1}, {1, 0}, {-1, 1}}); st != (UpsertStats{Unchanged: 3}) {
		t.Fatalf("re-upsert with new vectors stats = %+v", st)
	}
	if f.Len() != 3 {
		t.Fatalf("Len = %d, want 3", f.Len())
	}
	if after := mustSearch(t, f, []float32{1, 0.2}, 10, -1); !reflect.DeepEqual(before, after) {
		t.Fatalf("re-indexing changed results:\n before %v\n after  %v", before, after)
	}
	// The same text twice in one document is one chunk.
	dup := chunksOf("d.md", "same", "same")
	if st := mustUpsert(t, f, "d.md", dup, [][]float32{{1, 0}, {1, 0}}); st != (UpsertStats{Added: 1}) {
		t.Fatalf("duplicate chunk stats = %+v, want Added 1", st)
	}
}

func TestUpsertReplacesOnlyItsOwnSource(t *testing.T) {
	f := newIndex(t, 2)
	mustUpsert(t, f, "a.md", chunksOf("a.md", "keep", "old"), [][]float32{{1, 0}, {0, 1}})
	mustUpsert(t, f, "b.md", chunksOf("b.md", "old", "other"), [][]float32{{0, 1}, {1, 1}})

	st := mustUpsert(t, f, "a.md", chunksOf("a.md", "keep", "new"), [][]float32{{1, 0}, {-1, 1}})
	if st != (UpsertStats{Added: 1, Removed: 1, Unchanged: 1}) {
		t.Fatalf("stats = %+v, want 1 added, 1 removed, 1 unchanged", st)
	}
	got := keys(mustSearch(t, f, []float32{0, 1}, 10, -1))
	want := []string{"b.md|old", "a.md|new", "b.md|other", "a.md|keep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after replacing a.md: %v, want %v", got, want)
	}
	if f.Len() != 4 {
		t.Fatalf("Len = %d, want 4", f.Len())
	}
	// An empty chunk set empties the source and nothing else.
	if st := mustUpsert(t, f, "a.md", nil, nil); st != (UpsertStats{Removed: 2}) {
		t.Fatalf("empty upsert stats = %+v", st)
	}
	if got := keys(mustSearch(t, f, []float32{0, 1}, 10, -1)); !reflect.DeepEqual(got, []string{"b.md|old", "b.md|other"}) {
		t.Fatalf("after emptying a.md: %v", got)
	}
}

func TestDeleteRemovesOnlyThatSource(t *testing.T) {
	f := newIndex(t, 2)
	mustUpsert(t, f, "a.md", chunksOf("a.md", "1", "2", "3"), [][]float32{{1, 0}, {0, 1}, {1, 1}})
	mustUpsert(t, f, "b.md", chunksOf("b.md", "1", "2"), [][]float32{{1, 0}, {0, 1}})
	if n := f.Delete("a.md"); n != 3 {
		t.Fatalf("Delete(a.md) = %d, want 3", n)
	}
	if n := f.Delete("a.md"); n != 0 {
		t.Fatalf("second Delete(a.md) = %d, want 0", n)
	}
	if n := f.Delete("nope.md"); n != 0 {
		t.Fatalf("Delete(nope.md) = %d, want 0", n)
	}
	if got := keys(mustSearch(t, f, []float32{1, 0}, 10, -1)); !reflect.DeepEqual(got, []string{"b.md|1", "b.md|2"}) {
		t.Fatalf("after delete: %v", got)
	}
	if f.Len() != 2 {
		t.Fatalf("Len = %d, want 2", f.Len())
	}
}

func TestNewRejectsABadDimension(t *testing.T) {
	for _, d := range []int{0, -1} {
		noPanic(t, fmt.Sprintf("New(%d)", d), func() {
			if _, err := New(d); !errors.Is(err, ErrDimension) {
				t.Errorf("New(%d) err = %v, want ErrDimension", d, err)
			}
		})
	}
}

func TestUpsertRejectsBadInputAndChangesNothing(t *testing.T) {
	nan := float32(math.NaN())
	inf := float32(math.Inf(1))
	good := chunksOf("s.md", "one", "two")
	cases := []struct {
		name   string
		source string
		chunks []Chunk
		vecs   [][]float32
		want   error
	}{
		{"short vector", "s.md", good, [][]float32{{1, 0, 0}, {1, 0}}, ErrDimension},
		{"long vector", "s.md", good, [][]float32{{1, 0, 0}, {1, 0, 0, 0}}, ErrDimension},
		{"zero vector", "s.md", good, [][]float32{{1, 0, 0}, {0, 0, 0}}, ErrZeroVector},
		{"NaN", "s.md", good, [][]float32{{1, 0, 0}, {nan, 1, 0}}, ErrNotFinite},
		{"Inf", "s.md", good, [][]float32{{1, 0, 0}, {inf, 1, 0}}, ErrNotFinite},
		{"fewer vecs than chunks", "s.md", good, [][]float32{{1, 0, 0}}, ErrBadInput},
		{"empty source", "", chunksOf("", "one"), [][]float32{{1, 0, 0}}, ErrBadInput},
		{"stale chunk ID", "s.md", chunksOf("other.md", "one"), [][]float32{{1, 0, 0}}, ErrBadInput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newIndex(t, 3)
			mustUpsert(t, f, "s.md", chunksOf("s.md", "kept"), [][]float32{{0, 0, 1}})
			before := mustSearch(t, f, []float32{0, 0, 1}, 10, -1)
			noPanic(t, "Upsert", func() {
				if _, err := f.Upsert(c.source, c.chunks, c.vecs); !errors.Is(err, c.want) {
					t.Errorf("err = %v, want %v", err, c.want)
				}
			})
			if after := mustSearch(t, f, []float32{0, 0, 1}, 10, -1); !reflect.DeepEqual(before, after) || f.Len() != 1 {
				t.Errorf("a rejected upsert changed the index: before %v, after %v, Len %d", keys(before), keys(after), f.Len())
			}
		})
	}
}

func TestSearchRejectsBadInput(t *testing.T) {
	f := newIndex(t, 3)
	mustUpsert(t, f, "s.md", chunksOf("s.md", "one"), [][]float32{{1, 0, 0}})
	nan := float32(math.NaN())
	cases := []struct {
		name string
		q    []float32
		k    int
		min  float32
		want error
	}{
		{"short query", []float32{1, 0}, 3, 0, ErrDimension},
		{"nil query", nil, 3, 0, ErrDimension},
		{"zero query", []float32{0, 0, 0}, 3, 0, ErrZeroVector},
		{"NaN query", []float32{nan, 1, 0}, 3, 0, ErrNotFinite},
		{"Inf query", []float32{float32(math.Inf(-1)), 1, 0}, 3, 0, ErrNotFinite},
		{"NaN minScore", []float32{1, 0, 0}, 3, nan, ErrNotFinite},
		{"k = 0", []float32{1, 0, 0}, 0, 0, ErrBadK},
		{"k < 0", []float32{1, 0, 0}, -1, 0, ErrBadK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			noPanic(t, "Search", func() {
				hits, err := f.Search(c.q, c.k, c.min)
				if !errors.Is(err, c.want) {
					t.Errorf("err = %v, want %v", err, c.want)
				}
				if hits != nil {
					t.Errorf("hits = %v on error, want nil", keys(hits))
				}
			})
		})
	}
	// An empty index is not an error.
	empty := newIndex(t, 3)
	if hits := mustSearch(t, empty, []float32{1, 0, 0}, 3, -1); len(hits) != 0 {
		t.Fatalf("empty index hits = %v", keys(hits))
	}
}

// TestConcurrentUseIsSafe is meant for -race: writers upsert and delete their
// own sources while readers search and count. Without the lock the race
// detector fails it; with the lock every reader sees whole sources only.
func TestConcurrentUseIsSafe(t *testing.T) {
	f := newIndex(t, 4)
	const writers, readers, rounds = 4, 4, 200
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			src := fmt.Sprintf("w%d.md", w)
			for r := 0; r < rounds; r++ {
				texts := []string{fmt.Sprintf("a%d", r), fmt.Sprintf("b%d", r), "stable"}
				vecs := [][]float32{{1, float32(r), 0, 1}, {0, 1, float32(w), 1}, {1, 1, 1, 1}}
				if _, err := f.Upsert(src, chunksOf(src, texts...), vecs); err != nil {
					t.Errorf("Upsert: %v", err)
					return
				}
				if r%7 == 0 {
					f.Delete(src)
				}
			}
		}(w)
	}
	for rd := 0; rd < readers; rd++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				hits, err := f.Search([]float32{1, 1, 1, 1}, 50, -1)
				if err != nil {
					t.Errorf("Search: %v", err)
					return
				}
				// Each writer's source holds 0 or 3 chunks at any instant.
				per := map[string]int{}
				for _, h := range hits {
					per[h.Source]++
				}
				for src, n := range per {
					if n != 3 {
						t.Errorf("saw %d chunks of %s, want a whole source (3)", n, src)
						return
					}
				}
				if n := f.Len(); n%3 != 0 || n > 3*writers {
					t.Errorf("Len = %d mid-flight", n)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := f.Len(); n != 3*writers {
		t.Fatalf("final Len = %d, want %d", n, 3*writers)
	}
}
