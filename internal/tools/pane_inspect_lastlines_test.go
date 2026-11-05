// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"strings"
	"testing"
)

// channels' review of N2: lastLines never panics (n <= 0, no trailing
// newline), and the tail is capped in bytes, so one huge line cannot flood
// the model.
func TestLastLinesEdges(t *testing.T) {
	for _, n := range []int{0, -1} {
		got, truncated := lastLines("a\nb\nc", n)
		if got != "" || !truncated {
			t.Errorf("n=%d: got %q truncated=%v, want empty and truncated", n, got, truncated)
		}
	}
	if got, truncated := lastLines("", 5); got != "" || truncated {
		t.Errorf("empty output: %q %v", got, truncated)
	}
	if got, truncated := lastLines("only line", 3); got != "only line" || truncated {
		t.Errorf("one line without newline: %q %v", got, truncated)
	}
	huge := strings.Repeat("x", 3*paneInspectMaxTailBytes) + "END"
	got, truncated := lastLines(huge, 50)
	if len(got) > paneInspectMaxTailBytes || !truncated || !strings.HasSuffix(got, "END") {
		t.Errorf("one huge line: %d bytes, truncated=%v, suffix kept=%v", len(got), truncated, strings.HasSuffix(got, "END"))
	}
}
