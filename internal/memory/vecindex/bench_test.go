// SPDX-License-Identifier: Apache-2.0

package vecindex

import (
	"fmt"
	"math/rand"
	"testing"
)

// BenchmarkSearch measures exact search over n random vectors (top 10). The
// numbers feed design 010 §5's corpus-size threshold for adding an ANN.
func BenchmarkSearch(b *testing.B) {
	for _, dim := range []int{384, 768} {
		for _, n := range []int{10_000, 100_000} {
			b.Run(fmt.Sprintf("n=%d/dim=%d", n, dim), func(b *testing.B) {
				rng := rand.New(rand.NewSource(1))
				f, err := New(dim)
				if err != nil {
					b.Fatal(err)
				}
				vec := func() []float32 {
					v := make([]float32, dim)
					for i := range v {
						v[i] = float32(rng.NormFloat64())
					}
					return v
				}
				const perSource = 100
				for s := 0; s < n/perSource; s++ {
					src := fmt.Sprintf("s%05d.md", s)
					texts := make([]string, perSource)
					vecs := make([][]float32, perSource)
					for i := range texts {
						texts[i] = fmt.Sprintf("chunk %d", i)
						vecs[i] = vec()
					}
					if _, err := f.Upsert(src, chunksOf(src, texts...), vecs); err != nil {
						b.Fatal(err)
					}
				}
				if f.Len() != n {
					b.Fatalf("Len = %d, want %d", f.Len(), n)
				}
				q := vec()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := f.Search(q, 10, -1); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
