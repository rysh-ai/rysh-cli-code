package replay

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// castEvents parses every event line of a .cast file as [time, kind, data].
func castEvents(t *testing.T, path string) [][3]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Scan() // header
	var out [][3]any
	for sc.Scan() {
		var raw []any
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil {
			t.Fatalf("event line %q: %v", sc.Text(), err)
		}
		if len(raw) != 3 {
			t.Fatalf("event line %q: want 3 elements", sc.Text())
		}
		out = append(out, [3]any{raw[0], raw[1], raw[2]})
	}
	return out
}

// TestExportEmitsMidRecordingResizeEvents (design 006, B13): a pane that
// resizes DURING the recording exports the change as an asciicast "r" event at
// the right offset, with the header carrying the size in effect at the first
// output — not the final size stretched over the whole cast.
func TestExportEmitsMidRecordingResizeEvents(t *testing.T) {
	c := NewCapture(nil, msg.DefaultCodecRegistry(), "sess")
	c.byPane["p"] = []recEvent{
		{ts: 1000, text: "before\n"},
		{ts: 5000, text: "after\n"},
	}
	c.resizes["p"] = []resizeRec{
		{ts: 500, d: dim{cols: 100, rows: 30}},  // before first output → header
		{ts: 3000, d: dim{cols: 120, rows: 40}}, // mid-recording → "r" event
		{ts: 4000, d: dim{cols: 120, rows: 40}}, // same size → deduped, no event
	}
	c.dims["p"] = dim{cols: 120, rows: 40}

	path := filepath.Join(t.TempDir(), "r.cast")
	if _, err := c.ExportToFile("p", path, "t"); err != nil {
		t.Fatal(err)
	}

	if h := castHeader(t, path); h.Width != 100 || h.Height != 30 {
		t.Fatalf("header = %dx%d, want the size at first output (100x30), not the final size", h.Width, h.Height)
	}
	evs := castEvents(t, path)
	if len(evs) != 3 {
		t.Fatalf("want 2 output + 1 resize events, got %d: %v", len(evs), evs)
	}
	if evs[0][1] != "o" || evs[0][2] != "before\n" {
		t.Fatalf("first event = %v, want the first output", evs[0])
	}
	if evs[1][1] != "r" || evs[1][2] != "120x40" || evs[1][0].(float64) != 2.0 {
		t.Fatalf("second event = %v, want [2.0 r 120x40]", evs[1])
	}
	if evs[2][1] != "o" || evs[2][2] != "after\n" {
		t.Fatalf("third event = %v, want the second output", evs[2])
	}
}

// TestMergedExportHasNoResizeEvents: the all-panes export cannot meaningfully
// interleave several panes' sizes into one virtual terminal, so it emits no
// "r" events (header-only sizing, unchanged behaviour).
func TestMergedExportHasNoResizeEvents(t *testing.T) {
	c := NewCapture(nil, msg.DefaultCodecRegistry(), "sess")
	c.byPane["a"] = []recEvent{{ts: 1000, text: "a\n"}}
	c.byPane["b"] = []recEvent{{ts: 2000, text: "b\n"}}
	c.resizes["a"] = []resizeRec{{ts: 1500, d: dim{cols: 90, rows: 25}}}
	c.lastDims = dim{cols: 90, rows: 25}

	path := filepath.Join(t.TempDir(), "m.cast")
	if _, err := c.ExportToFile("", path, "t"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range castEvents(t, path) {
		if ev[1] == "r" {
			t.Fatalf("merged export emitted a resize event: %v", ev)
		}
	}
}

// TestOnResizedRecordsTimeline: real .resized wire messages append to the
// pane's resize timeline (not just the latest-dims map).
func TestOnResizedRecordsTimeline(t *testing.T) {
	c := NewCapture(nil, msg.DefaultCodecRegistry(), "sess")
	c.onResized(resizeMsg(t, "sess.pane.p.resized", "p", 100, 30))
	c.onResized(resizeMsg(t, "sess.pane.p.resized", "p", 120, 40))

	c.mu.Lock()
	tl := append([]resizeRec(nil), c.resizes["p"]...)
	c.mu.Unlock()
	if len(tl) != 2 || tl[0].d != (dim{cols: 100, rows: 30}) || tl[1].d != (dim{cols: 120, rows: 40}) {
		t.Fatalf("timeline = %+v, want both resizes in order", tl)
	}
	if tl[0].ts == 0 || tl[1].ts < tl[0].ts {
		t.Fatalf("timeline timestamps not monotonic: %+v", tl)
	}
}
