// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"reflect"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func TestVTRowDeltaOnlyChangedAndNewRows(t *testing.T) {
	prev := []string{"a", "b", "c"}
	next := []string{"a", "B", "c", "d"}
	got := VTRowDelta(next, VTRowHashes(prev))
	want := []VTLineDelta{{Y: 1, S: "B"}, {Y: 3, S: "d"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("VTRowDelta = %+v, want %+v", got, want)
	}
	if d := VTRowDelta(prev, VTRowHashes(prev)); len(d) != 0 {
		t.Fatalf("identical screen must give an empty delta, got %+v", d)
	}
}

func TestVTRowsHashDependsOnRowsAndCount(t *testing.T) {
	a := VTRowsHash(VTRowHashes([]string{"x", "y"}))
	if a != VTRowsHash(VTRowHashes([]string{"x", "y"})) {
		t.Fatal("same screen must hash the same")
	}
	if a == VTRowsHash(VTRowHashes([]string{"y", "x"})) {
		t.Fatal("row order must change the hash")
	}
	if a == VTRowsHash(VTRowHashes([]string{"x", "y", ""})) {
		t.Fatal("an extra empty row must change the hash")
	}
}

func TestPaneContentHashFields(t *testing.T) {
	base := domain.PaneSnapshot{Output: "o", VTScreen: []string{"r"}, VTCursorRow: 1}
	h := PaneContentHash(base)
	for name, s := range map[string]domain.PaneSnapshot{
		"output": {Output: "o2", VTScreen: []string{"r"}, VTCursorRow: 1},
		"screen": {Output: "o", VTScreen: []string{"r2"}, VTCursorRow: 1},
		"cursor": {Output: "o", VTScreen: []string{"r"}, VTCursorRow: 2},
		"mode":   {Output: "o", VTScreen: []string{"r"}, VTCursorRow: 1, ModeOutputs: map[string]string{"m": "x"}},
	} {
		if PaneContentHash(s) == h {
			t.Errorf("%s change must change the hash", name)
		}
	}
	// ID and other non-display fields do not affect it.
	same := base
	same.ID = "other"
	if PaneContentHash(same) != h {
		t.Error("non-display fields must not change the hash")
	}
}
