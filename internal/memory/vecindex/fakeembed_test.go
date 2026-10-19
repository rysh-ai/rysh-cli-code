// SPDX-License-Identifier: Apache-2.0

package vecindex

import (
	"context"
	"hash/fnv"
	"strings"
	"testing"
	"unicode"
)

// fakeEmbedder is a deterministic bag-of-words embedder for tests only. Each
// lower-cased word that is not a stop word adds ±1 to one of dim buckets,
// chosen by its FNV-1a hash. Texts that share words get a positive cosine;
// texts that share none score near zero. It needs no network and no model.
type fakeEmbedder struct{ dim int }

var _ Embedder = fakeEmbedder{}

var fakeStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "the": true, "is": true, "it": true,
	"to": true, "of": true, "for": true, "in": true, "on": true, "with": true,
	"that": true, "this": true, "be": true, "i": true, "do": true, "how": true,
}

func (e fakeEmbedder) Dim() int { return e.dim }

func (e fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, e.dim)
		words := strings.FieldsFunc(strings.ToLower(t), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		for _, w := range words {
			if fakeStopWords[w] {
				continue
			}
			h := fnv.New64a()
			h.Write([]byte(w))
			sum := h.Sum64()
			sign := float32(1)
			if sum>>63 == 1 {
				sign = -1
			}
			v[sum%uint64(e.dim)] += sign
		}
		out[i] = v
	}
	return out, nil
}

func TestFakeEmbedderIsDeterministic(t *testing.T) {
	e := fakeEmbedder{dim: 64}
	a, _ := e.Embed(context.Background(), []string{"Sourdough bread, sourdough!"})
	b, _ := e.Embed(context.Background(), []string{"sourdough BREAD sourdough"})
	if len(a[0]) != 64 {
		t.Fatalf("dim = %d, want 64", len(a[0]))
	}
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			t.Fatalf("component %d: %v != %v; case and punctuation must not matter", i, a[0][i], b[0][i])
		}
	}
}
