package replay

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func resizeMsg(t *testing.T, subject, paneID string, cols, rows int) *nats.Msg {
	t.Helper()
	payload, err := json.Marshal(&msg.MsgPaneResized{PaneID: paneID, Cols: cols, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(msg.NATSEnvelope{TypeTag: msg.TagPaneResized, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return &nats.Msg{Subject: subject, Data: env}
}

func castHeader(t *testing.T, path string) Header {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Scan()
	var h Header
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
		t.Fatalf("header: %v", err)
	}
	return h
}

// TestExportUsesRealPaneDims is the design-006 fix: the asciicast header must
// carry the pane's real terminal size (learned from its .resized events), not a
// hardcoded 80x24.
func TestExportUsesRealPaneDims(t *testing.T) {
	codecs := msg.DefaultCodecRegistry()
	c := NewCapture(nil, codecs, "sess")
	c.byPane["pane-a"] = []recEvent{{ts: 1000, text: "hi\n"}}

	// The pane resizes to 120x40 — delivered exactly as PaneActor publishes it.
	c.onResized(resizeMsg(t, "sess.pane.pane-a.resized", "pane-a", 120, 40))

	path := filepath.Join(t.TempDir(), "a.cast")
	if _, err := c.ExportToFile("pane-a", path, "t"); err != nil {
		t.Fatal(err)
	}
	if h := castHeader(t, path); h.Width != 120 || h.Height != 40 {
		t.Fatalf("header dims = %dx%d, want 120x40 (hardcoded 80x24 not replaced)", h.Width, h.Height)
	}

	// The merged ("" = all panes) export uses the most-recent size seen.
	c.onResized(resizeMsg(t, "sess.pane.pane-b.resized", "pane-b", 200, 50))
	c.byPane["pane-b"] = []recEvent{{ts: 1500, text: "yo\n"}}
	allPath := filepath.Join(t.TempDir(), "all.cast")
	if _, err := c.ExportToFile("", allPath, "all"); err != nil {
		t.Fatal(err)
	}
	if h := castHeader(t, allPath); h.Width != 200 || h.Height != 50 {
		t.Fatalf("merged header dims = %dx%d, want 200x50 (last-seen)", h.Width, h.Height)
	}
}

// TestExportFallsBackWhenNoResize: with no resize ever seen, the writer's 80x24
// default stands — the change must not break the no-dimensions case.
func TestExportFallsBackWhenNoResize(t *testing.T) {
	c := NewCapture(nil, nil, "sess")
	c.byPane["pane-a"] = []recEvent{{ts: 1000, text: "hi\n"}}
	path := filepath.Join(t.TempDir(), "a.cast")
	if _, err := c.ExportToFile("pane-a", path, "t"); err != nil {
		t.Fatal(err)
	}
	if h := castHeader(t, path); h.Width != 80 || h.Height != 24 {
		t.Fatalf("no-dims fallback = %dx%d, want 80x24", h.Width, h.Height)
	}
}
