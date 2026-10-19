// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// F-70: the wrap cache re-wrapped a pane's whole buffer (up to 256 KB) on every
// streamed chunk. It now reuses the rows of the complete lines a grown buffer
// shares with the cached one. The one property that matters: the cached result
// is always exactly what a from-scratch wrap produces.

func randomChunk(r *rand.Rand) string {
	var b strings.Builder
	for i, n := 0, r.Intn(6)+1; i < n; i++ {
		switch r.Intn(8) {
		case 0:
			b.WriteString("\n")
		case 1:
			b.WriteString("\x1b[31mred\x1b[0m ")
		case 2:
			b.WriteString(strings.Repeat("long-word ", r.Intn(30)))
		case 3:
			b.WriteString("\n\x02right-aligned echo")
		case 4:
			b.WriteString("wide 世界 ")
		default:
			fmt.Fprintf(&b, "tok%d ", r.Intn(1000))
		}
	}
	return b.String()
}

func TestWrapRowsCachedMatchesAFullWrap(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	m := Model{paneWrapCache: map[string]wrapCacheEntry{}}
	out, width := "", 40
	for step := 0; step < 3000; step++ {
		switch k := r.Intn(40); {
		case k == 0:
			width = 10 + r.Intn(70) // resize
		case k == 1:
			out = randomChunk(r) // unrelated replacement (mode switch, backfill)
		case k < 6:
			out = capTail(out+randomChunk(r), 2000) // append at the cap: front trimmed
		default:
			out += randomChunk(r) // plain append
		}
		got := m.wrapRowsCached("p", out, width)
		want := wrapRows(out, width)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("step %d (width %d): cached wrap diverged from a full wrap\n got %d rows\nwant %d rows",
				step, width, len(got), len(want))
		}
	}
}

func TestWrapIncrementalReusesCompleteLines(t *testing.T) {
	base := strings.Repeat("a line that is long enough to wrap at width twenty\n", 200)
	old := wrapFull(base+"partial", 20)

	for name, next := range map[string]string{
		"append":           base + "partial line, now finished\nand more",
		"front-trimmed":    capTail(base+"partial line\nnext", len(base)-500),
		"append after \\n": base + "partial\n",
	} {
		e, ok := wrapIncremental(old, next)
		if !ok {
			t.Fatalf("%s: incremental path not taken for a buffer that continues the cached one", name)
		}
		if want := wrapRows(next, 20); !reflect.DeepEqual(e.rows, want) {
			t.Fatalf("%s: incremental rows differ from a full wrap", name)
		}
	}
	if _, ok := wrapIncremental(old, "something else entirely"); ok {
		t.Fatal("incremental path taken for a buffer that does not continue the cached one")
	}
}

// BenchmarkWrapStreamingFullPane is the F-70 workload: a pane at the 256 KB
// cap receiving one small chunk per frame.
func BenchmarkWrapStreamingFullPane(b *testing.B) {
	buf := capTail(strings.Repeat("\x1b[32m$\x1b[0m go test ./... ok  rysh/internal/actors  12.3s  some wrapped trailing text\n", 4000), 256*1024)
	for _, mode := range []string{"cached", "full"} {
		b.Run(mode, func(b *testing.B) {
			m := Model{paneWrapCache: map[string]wrapCacheEntry{}}
			out := buf
			m.wrapRowsCached("p", out, 80)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out = capTail(out+fmt.Sprintf("chunk %d\n", i), 256*1024)
				if mode == "cached" {
					m.wrapRowsCached("p", out, 80)
				} else {
					wrapRows(out, 80)
				}
			}
		})
	}
}
