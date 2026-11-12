// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"strings"
	"testing"
	"time"
)

// C4 (rr-order-154): the workspace's sandboxes as one board group: awake runs
// first (size, units, time left, attached), then suspended, then the last few
// ended ones; a header with the units in use.

func TestRenderBoard(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	runs := []Run{
		{ID: "r1", Name: "api-tests", State: "running", Size: "large", ExpiresAt: now.Add(25 * time.Minute)},
		{ID: "r2", Name: "classic", State: "running", ExpiresAt: now.Add(3*time.Hour + 5*time.Minute)},
		{ID: "r3", Name: "family", State: "suspended", Size: "default", ExpiresAt: now.Add(48 * time.Hour)},
		{ID: "r4", Name: "old", State: "expired", Reason: "idle timeout", ExpiresAt: now.Add(-time.Hour)},
		{ID: "r5", Name: "pending-one", State: "pending", Size: "small", ExpiresAt: now.Add(5 * time.Minute)},
	}
	out := RenderBoard(runs, now, map[string]bool{"r1": true})
	for _, want := range []string{
		"sandboxes: 3 awake (6 units), 1 suspended, 1 ended",
		"AWAKE", "api-tests", "large", "4u", "25m left", "attached",
		"classic", "-", "1u", "3h05m left",
		"pending-one", "small", "pending",
		"SUSPENDED", "family", "default",
		"ENDED", "old", "idle timeout",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("board lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "AWAKE") > strings.Index(out, "SUSPENDED") || strings.Index(out, "SUSPENDED") > strings.Index(out, "ENDED") {
		t.Errorf("group order wrong:\n%s", out)
	}
	if got := RenderBoard(nil, now, nil); !strings.Contains(got, "no sandboxes") {
		t.Errorf("empty board = %q", got)
	}
}

func TestRenderBoardKeepsOnlyTheLastEnded(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	var runs []Run
	for i := 0; i < 12; i++ {
		runs = append(runs, Run{ID: string(rune('a' + i)), Name: "e" + string(rune('a'+i)), State: "stopped", CreatedAt: now.Add(time.Duration(i) * time.Minute)})
	}
	out := RenderBoard(runs, now, nil)
	if strings.Contains(out, "ea") || !strings.Contains(out, "el") || !strings.Contains(out, "(+7 older)") {
		t.Errorf("want the 5 newest ended and a count of the rest:\n%s", out)
	}
}

func TestSizeUnits(t *testing.T) {
	for s, u := range map[string]int{"": 1, "small": 1, "default": 2, "large": 4, "xlarge": 8, "unknown": 1} {
		if got := SizeUnits(s); got != u {
			t.Errorf("SizeUnits(%q) = %d, want %d", s, got, u)
		}
	}
}
