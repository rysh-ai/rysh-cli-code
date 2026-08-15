// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCast(t *testing.T) {
	var sb strings.Builder
	err := WriteCast(&sb, Header{Timestamp: 1700000000, Title: "t"}, []Event{
		{Time: 0, Data: "hello "},
		{Time: 1.5, Data: "world\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(strings.NewReader(sb.String()))
	sc.Scan()
	var h Header
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
		t.Fatalf("header: %v", err)
	}
	if h.Version != 2 || h.Width != 80 || h.Height != 24 || h.Timestamp != 1700000000 {
		t.Fatalf("header = %+v", h)
	}
	// First event line.
	sc.Scan()
	var e0 []interface{}
	if err := json.Unmarshal(sc.Bytes(), &e0); err != nil {
		t.Fatalf("event: %v", err)
	}
	if e0[1] != "o" || e0[2] != "hello " {
		t.Fatalf("event0 = %v", e0)
	}
}

func TestCaptureExport(t *testing.T) {
	c := NewCapture(nil, nil, "sess")
	// Seed events directly (unexported, same package).
	c.byPane["pane-a"] = []recEvent{
		{ts: 1000, text: "$ ls\n"},
		{ts: 2500, text: "file.go\n"},
	}
	c.byPane["pane-b"] = []recEvent{{ts: 1500, text: "hi\n"}}

	if c.Count("") != 3 || c.Count("pane-a") != 2 {
		t.Fatalf("counts: all=%d a=%d", c.Count(""), c.Count("pane-a"))
	}

	// Export pane-a: deltas relative to first (1000ms).
	path := filepath.Join(t.TempDir(), "a.cast")
	n, err := c.ExportToFile("pane-a", path, "title")
	if err != nil || n != 2 {
		t.Fatalf("export: n=%d err=%v", n, err)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 { // header + 2 events
		t.Fatalf("lines = %d", len(lines))
	}
	var e1 []interface{}
	_ = json.Unmarshal([]byte(lines[2]), &e1)
	if e1[0].(float64) != 1.5 { // (2500-1000)/1000
		t.Fatalf("event1 time = %v, want 1.5", e1[0])
	}

	// Export all panes merged, sorted by ts (pane-a@1000, pane-b@1500, pane-a@2500).
	allPath := filepath.Join(t.TempDir(), "all.cast")
	n, err = c.ExportToFile("", allPath, "all")
	if err != nil || n != 3 {
		t.Fatalf("export all: n=%d err=%v", n, err)
	}
}

func TestPaneFromSubject(t *testing.T) {
	if got := paneFromSubject("mysession.pane.abc-123.output"); got != "abc-123" {
		t.Fatalf("got %q", got)
	}
	if got := paneFromSubject("nope"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestExportEmpty(t *testing.T) {
	c := NewCapture(nil, nil, "s")
	if _, err := c.ExportToFile("nope", filepath.Join(t.TempDir(), "x.cast"), "t"); err == nil {
		t.Fatal("expected error on empty capture")
	}
}
